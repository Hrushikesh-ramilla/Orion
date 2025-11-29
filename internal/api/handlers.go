// Package api implements the HTTP REST interface for the Distributed
// Task Orchestrator. It exposes endpoints for submitting DAGs and
// querying system status.
package api

import (
	"encoding/json"
	"log"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"go-enterprise-scheduler/internal/engine"
	"go-enterprise-scheduler/internal/storage"
	"go-enterprise-scheduler/pkg/models"
	
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
)

// -----------------------------------------------------------------
// Handler wires the HTTP routes to the engine and storage layers.
// -----------------------------------------------------------------

// Handler holds references to shared dependencies and registers HTTP routes.
type Handler struct {
	scheduler *engine.Scheduler
	wal       *storage.WAL
	mu        sync.Mutex
	clients   map[string]*rate.Limiter
	idemStore *IdempotencyStore
}

// NewHandler creates a Handler and returns a configured http.ServeMux
// with all API routes registered.
func NewHandler(scheduler *engine.Scheduler, wal *storage.WAL, idemStore *IdempotencyStore) http.Handler {
	h := &Handler{
		scheduler: scheduler,
		wal:       wal,
		clients:   make(map[string]*rate.Limiter),
		idemStore: idemStore,
	}

	mux := http.NewServeMux()

	// POST /api/v1/dag — Submit a DAG of tasks for scheduling.
	mux.HandleFunc("/api/v1/dag", h.handleSubmitDAG)

	// GET /api/v1/status — Retrieve current system metrics.
	mux.HandleFunc("/api/v1/status", h.handleStatus)

	// GET /healthz — Liveness probe (always 200).
	mux.HandleFunc("/healthz", h.handleHealthz)

	// GET /readyz — Readiness probe (200 only if scheduler + WAL ready).
	mux.HandleFunc("/readyz", h.handleReadyz)

	// GET /metrics — Prometheus metrics endpoint natively scraped
	mux.Handle("/metrics", promhttp.Handler())

	return mux
}

func (h *Handler) getLimiter(ip string) *rate.Limiter {
	h.mu.Lock()
	defer h.mu.Unlock()

	limiter, exists := h.clients[ip]
	if !exists {
		limiter = rate.NewLimiter(5, 10) // stricter per-client
		h.clients[ip] = limiter
	}
	return limiter
}

// -----------------------------------------------------------------
// POST /api/v1/dag
// -----------------------------------------------------------------
// Accepts a JSON array of Task objects. The tasks are first persisted
// to the WAL for crash recovery, then injected into the DAG scheduler.
//
// Request body example:
//
//	[
//	  {"id": "build", "payload": "go build ./...", "priority": 1, "dependencies": []},
//	  {"id": "test",  "payload": "go test ./...",  "priority": 2, "dependencies": ["build"]}
//	]
//
// Responses:
//
//	201 Created  — Tasks accepted and queued.
//	400 Bad Request — Malformed JSON body.
//	405 Method Not Allowed — Non-POST request.
//	500 Internal Server Error — WAL or scheduler failure.
func (h *Handler) handleSubmitDAG(w http.ResponseWriter, r *http.Request) {
	log.Println("API: request received")
	defer log.Println("API: request completed")

	// Enforce POST method.
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip != "127.0.0.1" && ip != "::1" {
		limiter := h.getLimiter(ip)
		if !limiter.Allow() {
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
	}

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		http.Error(w, `{"error":"missing Idempotency-Key header"}`, http.StatusBadRequest)
		return
	}

	if res, ok := h.idemStore.Get(key); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(res.Response)
		return
	}

	// Prevent huge JSON bombs. Max 1MB
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Decode the incoming task array.
	var tasks []models.Task
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields() // Strict parsing.
	if err := decoder.Decode(&tasks); err != nil {
		slog.Warn("bad request", "error", err)
		http.Error(w, `{"error":"invalid JSON body: `+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Validate: at least one task must be provided.
	if len(tasks) == 0 {
		http.Error(w, `{"error":"empty task list"}`, http.StatusBadRequest)
		return
	}

	// Emulate true execution latency coupling without hard-blocking API concurrency maps:
	// If internal pipeline crosses ~50 items, inject native execution backpressure.
	for h.scheduler.QueueSize() > 50 {
		time.Sleep(10 * time.Millisecond)
	}

	const MaxTasksPerRequest = 1000
	if len(tasks) > MaxTasksPerRequest {
		http.Error(w, `{"error":"too many tasks in request"}`, http.StatusBadRequest)
		return
	}

	const MaxDepsPerTask = 50
	for _, t := range tasks {
		if len(t.Dependencies) > MaxDepsPerTask {
			http.Error(w, `{"error":"too many dependencies"}`, http.StatusBadRequest)
			return
		}
	}

	const MaxQueueSize = 10000
	if h.scheduler.QueueSize() > MaxQueueSize {
		http.Error(w, `{"error":"system overloaded"}`, http.StatusTooManyRequests)
		return
	}

	// Step 1: Persist to WAL before processing (write-ahead guarantee).
	if err := h.wal.AppendIngest(tasks); err != nil {
		slog.Error("WAL ingest failed", "error", err)
		http.Error(w, `{"error":"persistence failure"}`, http.StatusInternalServerError)
		return
	}

	// Step 2: Inject into the DAG scheduler.
	if err := h.scheduler.Ingest(tasks); err != nil {
		slog.Error("scheduler ingest failed", "error", err)
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	slog.Info("accepted tasks into the DAG", "count", len(tasks))

	// Return 201 Created with a summary.
	respData := map[string]interface{}{
		"status":   "accepted",
		"ingested": len(tasks),
	}
	respBytes, _ := json.Marshal(respData)

	h.idemStore.Set(key, Result{
		Status:   "accepted",
		Response: respBytes,
	})
	_ = h.wal.AppendRequest(key)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(respBytes)
}

// -----------------------------------------------------------------
// GET /api/v1/status
// -----------------------------------------------------------------
// Returns a JSON object with the current system metrics: counts of
// tasks in pending, running, and completed states.
//
// Response example:
//
//	{"pending": 3, "running": 2, "completed": 5}
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Enforce GET method.
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	pending, running, completed, failed, retried := h.scheduler.Metrics()

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]int{
		"pending":   pending,
		"running":   running,
		"completed": completed,
		"failed":    failed,
		"retried":   retried,
	}
	json.NewEncoder(w).Encode(resp)
}

// -----------------------------------------------------------------
// GET /healthz
// -----------------------------------------------------------------
// Kubernetes-style liveness probe. Always returns 200 if the process
// is alive and serving HTTP.
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"alive"}`))
}

// -----------------------------------------------------------------
// GET /readyz
// -----------------------------------------------------------------
// Kubernetes-style readiness probe. Returns 200 only if both the
// scheduler event loop and the WAL are initialized and operational.
func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil || h.wal == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"status":"not ready","reason":"scheduler or WAL not initialized"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ready"}`))
}
