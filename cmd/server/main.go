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

	scheduler := engine.NewScheduler()
	slog.Info("DAG scheduler initialized")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler.Start(ctx)
	slog.Info("DAG scheduler event loop started")

	dispatcher := engine.NewDispatcher(scheduler, engine.DefaultWorkerCount)
	dispatcher.Start(ctx)
	slog.Info("dispatcher started", "workers", engine.DefaultWorkerCount)

	handler := api.NewHandler(scheduler)
	server := &http.Server{
		Addr:         ":8080",
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("HTTP server listening", "port", 8080)
		slog.Info("POST /api/v1/dag -> Submit a task DAG")
		slog.Info("GET  /api/v1/status -> System metrics")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	slog.Info("received signal, initiating graceful shutdown", "signal", sig)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server forced shutdown", "error", err)
	} else {
		slog.Info("HTTP server stopped gracefully")
	}

	cancel()
	dispatcher.Wait()

	slog.Info("all systems stopped. Goodbye.")
}
