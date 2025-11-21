package api

import (
	"encoding/json"
	"log"
	"log/slog"
	"net/http"

	"go-enterprise-scheduler/internal/engine"
	"go-enterprise-scheduler/internal/storage"
	"go-enterprise-scheduler/pkg/models"
)

type Handler struct {
	scheduler *engine.Scheduler
	wal       *storage.WAL
}

func NewHandler(scheduler *engine.Scheduler, wal *storage.WAL) http.Handler {
	h := &Handler{scheduler: scheduler, wal: wal}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/dag", h.handleSubmitDAG)
	mux.HandleFunc("/api/v1/status", h.handleStatus)
	return mux
}

func (h *Handler) handleSubmitDAG(w http.ResponseWriter, r *http.Request) {
	log.Println("API: request received")
	defer log.Println("API: request completed")

	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var tasks []models.Task
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&tasks); err != nil {
		slog.Warn("bad request", "error", err)
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if len(tasks) == 0 {
		http.Error(w, `{"error":"empty task list"}`, http.StatusBadRequest)
		return
	}

	// WAL first â€” write-ahead guarantee
	if err := h.wal.AppendIngest(tasks); err != nil {
		slog.Error("WAL ingest failed", "error", err)
		http.Error(w, `{"error":"persistence failure"}`, http.StatusInternalServerError)
		return
	}

	if err := h.scheduler.Ingest(tasks); err != nil {
		slog.Error("scheduler ingest failed", "error", err)
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	slog.Info("accepted tasks into the DAG", "count", len(tasks))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "accepted", "ingested": len(tasks),
	})
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	pending, running, completed := h.scheduler.Metrics()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{
		"pending": pending, "running": running, "completed": completed,
	})
}
