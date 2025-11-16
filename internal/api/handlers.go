package api

import (
	"encoding/json"
	"log"
	"log/slog"
	"net/http"

	"go-enterprise-scheduler/internal/engine"
	"go-enterprise-scheduler/pkg/models"
)

type Handler struct {
	scheduler *engine.Scheduler
}

func NewHandler(scheduler *engine.Scheduler) http.Handler {
	h := &Handler{scheduler: scheduler}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/dag", h.handleSubmitDAG)
	return mux
}

func (h *Handler) handleSubmitDAG(w http.ResponseWriter, r *http.Request) {
	log.Println("API: request received")

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

	if err := h.scheduler.Ingest(tasks); err != nil {
		slog.Error("scheduler ingest failed", "error", err)
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	slog.Info("accepted tasks into the DAG", "count", len(tasks))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "accepted",
		"ingested": len(tasks),
	})
}
