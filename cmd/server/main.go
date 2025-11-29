// Distributed Task Orchestrator — main.go
//
// This is the entry point for the go-enterprise-scheduler server.
// It wires together the storage layer (WAL), the DAG scheduling engine,
// the worker pool dispatcher, and the REST API, then starts the HTTP
// server on port 8080 with graceful shutdown support.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go-enterprise-scheduler/internal/api"
	"go-enterprise-scheduler/internal/engine"
	"go-enterprise-scheduler/internal/storage"
)

func main() {
	slog.Info("=== Distributed Task Orchestrator ===")
	slog.Info("Starting up...")

	// -----------------------------------------------------------------
	// 1. Initialize the Write-Ahead Log for crash recovery.
	// -----------------------------------------------------------------
	walPath := os.Getenv("WAL_PATH")
	if walPath == "" {
		walPath = "wal.json"
	}
	wal, err := storage.NewWAL(walPath)
	if err != nil {
		slog.Error("failed to initialize WAL", "error", err)
		os.Exit(1)
	}
	defer wal.Close()
	slog.Info("WAL initialized", "file", walPath)

	// -----------------------------------------------------------------
	// 2. Initialize the DAG scheduler.
	// -----------------------------------------------------------------
	scheduler := engine.NewScheduler(wal)
	slog.Info("DAG scheduler initialized")

	// -----------------------------------------------------------------
	// 3. Create a cancellable context for graceful shutdown.
	// -----------------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// -----------------------------------------------------------------
	// 4. Start the Scheduler's event loop to push tasks to workers.
	// -----------------------------------------------------------------
	scheduler.Start(ctx)
	slog.Info("DAG scheduler event loop started")

	// -----------------------------------------------------------------
	// 5. Replay WAL entries from any previous crash/shutdown.
	// -----------------------------------------------------------------
	state, err := wal.Recover()
	if err != nil {
		slog.Error("WAL recovery failed", "error", err)
		os.Exit(1)
	}
	if state != nil && len(state.Entries) > 0 {
		slog.Info("recovering WAL events", "count", len(state.Entries))
		for _, entry := range state.Entries {
			if entry.Type == "INGEST" {
				_ = scheduler.Ingest(entry.Tasks)
			} else if entry.Type == "START" {
				scheduler.ReplayStart(entry.TaskID)
			} else if entry.Type == "COMPLETE" {
				scheduler.ReplayComplete(entry.TaskID)
			} else if entry.Type == "FAIL" {
				scheduler.ReplayFail(entry.TaskID)
			}
		}
		scheduler.RequeueOrphans(state.InProgressTasks)
		slog.Info("WAL recovery complete", "orphans_requeued", len(state.InProgressTasks))
	} else {
		slog.Info("WAL is clean — no tasks to recover")
	}

	// -----------------------------------------------------------------
	// 6. Start the DAG Dispatch loop to pop tasks for workers.
	// -----------------------------------------------------------------
	scheduler.StartDispatch(ctx)
	slog.Info("DAG dispatch loop started")

	// -----------------------------------------------------------------
	// 7. Start the Dispatcher with a fixed worker pool.
	// -----------------------------------------------------------------
	dispatcher := engine.NewDispatcher(scheduler, wal, engine.DefaultWorkerCount)
	dispatcher.Start(ctx)
	slog.Info("dispatcher started", "workers", engine.DefaultWorkerCount)

	// -----------------------------------------------------------------
	// 8. Configure and start the HTTP server.
	// -----------------------------------------------------------------
	idemStore := api.NewIdempotencyStore()
	if state != nil {
		for _, entry := range state.Entries {
			if entry.Type == "REQUEST" {
				idemStore.Set(entry.IdempotencyKey, api.Result{
					Status:   "accepted",
					Response: []byte(`{"status":"duplicate request ignored (recovered)"}`),
				})
			}
		}
	}

	handler := api.NewHandler(scheduler, wal, idemStore)
	server := &http.Server{
		Addr:         ":8080",
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Run the HTTP server in a separate goroutine so we can listen
	// for OS signals on the main goroutine.
	go func() {
		slog.Info("HTTP server listening", "port", 8080)
		slog.Info("POST /api/v1/dag -> Submit a task DAG")
		slog.Info("GET /api/v1/status -> System metrics")
		slog.Info("GET /healthz -> Liveness probe")
		slog.Info("GET /readyz -> Readiness probe")
		slog.Info("GET /metrics -> Prometheus metrics")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// -----------------------------------------------------------------
	// 9. Graceful shutdown: wait for SIGINT or SIGTERM.
	// -----------------------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	slog.Info("received signal, initiating graceful shutdown", "signal", sig)

	// Phase 1: Stop accepting new HTTP connections, allow in-flight to finish.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server forced shutdown", "error", err)
	} else {
		slog.Info("HTTP server stopped gracefully")
	}

	// Phase 2: Cancel the dispatcher context so workers finish current tasks
	// and then exit.
	cancel()
	dispatcher.Wait()

	slog.Info("all systems stopped. Goodbye.")
}
