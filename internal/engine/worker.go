// Package engine — worker.go implements a fixed-size worker pool with a
// Dispatcher that continuously polls the Scheduler for ready tasks and
// fans them out to workers via a buffered channel.
package engine

import (
	"context"
	"log"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"go-enterprise-scheduler/internal/storage"
	"go-enterprise-scheduler/pkg/telemetry"
)

// -----------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------

// DefaultWorkerCount defines the number of concurrent workers in the pool.
const DefaultWorkerCount = 4

// -----------------------------------------------------------------
// Dispatcher manages the lifecycle of the worker pool.
// -----------------------------------------------------------------

// Dispatcher continuously polls the Scheduler for ready tasks and dispatches
// them to a fixed pool of worker goroutines via a shared channel.
//
// Usage:
//
//	d := engine.NewDispatcher(scheduler, 4)
//	d.Start(ctx)    // Launches workers + polling loop.
//	// ... submit tasks to the scheduler ...
//	d.Wait()        // Blocks until all workers drain and exit.
type Dispatcher struct {
	scheduler   *Scheduler
	wal         *storage.WAL
	workerCount int
	wg          sync.WaitGroup // Tracks active workers for graceful shutdown.
}

// NewDispatcher creates a Dispatcher with the specified number of workers.
func NewDispatcher(scheduler *Scheduler, wal *storage.WAL, workerCount int) *Dispatcher {
	return &Dispatcher{
		scheduler:   scheduler,
		wal:         wal,
		workerCount: workerCount,
	}
}

// Start launches the worker goroutines. Workers will wait for tasks pushed
// by the scheduler. The loop runs until context implies cancellation.
func (d *Dispatcher) Start(ctx context.Context) {
	// Launch the fixed-size worker pool.
	for i := 1; i <= d.workerCount; i++ {
		d.wg.Add(1)
		go d.worker(ctx, i)
	}

	slog.Info("dispatcher started", "workerCount", d.workerCount)
}

// Wait blocks until all workers and the polling loop have exited.
// Call this after cancelling the context to ensure graceful drain.
func (d *Dispatcher) Wait() {
	d.wg.Wait()
	slog.Info("all workers stopped")
}

// worker is the goroutine body for each pool member. It reads tasks from
// the shared scheduler channel, "executes" them (simulated), and notifies the
// scheduler upon completion so that downstream dependencies are unlocked.
func (d *Dispatcher) worker(ctx context.Context, id int) {
	defer d.wg.Done()

	slog.Info("worker online", "worker_id", id)

	for {
		select {
		case <-ctx.Done():
			slog.Info("worker offline (context cancelled)", "worker_id", id)
			return
		case task, ok := <-d.scheduler.ReadyTasks(): // Block on scheduler push
			if !ok {
				slog.Info("worker offline (channel closed)", "worker_id", id)
				return
			}
			log.Println("WORKER: picked task", task.ID)

			// GUARD 1: If failure cascade already marked this task cancelled
			// while it was sitting in readyChan, skip execution entirely.
			if task.Cancelled.Load() == 1 {
				slog.Warn("skipping cascade-failed task", "worker_id", id, "task_id", task.ID)
				continue
			}

			telemetry.ActiveWorkers.Inc()
			task.StartTime = time.Now()

			if err := d.wal.AppendStart(task.ID); err != nil {
				slog.Error("failed to write start event to WAL", "worker_id", id, "error", err)
			}

			slog.Info("executing task", "worker_id", id, "task_id", task.ID, "payload", task.Payload)

			// --- Simulate task execution ---
			execTime := time.Duration(50+rand.Intn(150)) * time.Millisecond
			if task.Payload == "sleep" {
				execTime = 2000 * time.Millisecond
			}

			select {
			case <-time.After(execTime):
				// Simulate explicit payload failure
				if task.Payload == "fail" {
					slog.Warn("simulated failure", "worker_id", id, "task_id", task.ID)
					if err := d.wal.AppendFail(task.ID); err != nil {
						slog.Error("failed to write fail event to WAL", "worker_id", id, "error", err)
						telemetry.ActiveWorkers.Dec()
						continue
					}
					d.scheduler.Fail(task.ID)
					telemetry.ActiveWorkers.Dec()
					continue
				}
				// Task execution completed successfully.
			case <-ctx.Done():
				slog.Warn("interrupted during execution", "worker_id", id, "task_id", task.ID)
				telemetry.ActiveWorkers.Dec()
				return
			}

			// GUARD 2: If failure cascade ran DURING execution,
			// do not mark this task as completed.
			if task.Cancelled.Load() == 1 {
				slog.Warn("task was cascade-failed during execution, skipping completion", "worker_id", id, "task_id", task.ID)
				telemetry.ActiveWorkers.Dec()
				continue
			}

			task.EndTime = time.Now()
			telemetry.TaskLatency.Observe(task.EndTime.Sub(task.StartTime).Seconds())

			// Notify the scheduler that this task is done
			if err := d.wal.AppendComplete(task.ID); err != nil {
				slog.Error("failed to write completion to WAL", "worker_id", id, "error", err)
			}
			d.scheduler.Complete(task.ID)
			log.Println("WORKER: finished task", task.ID)
			slog.Info("finished task", "worker_id", id, "task_id", task.ID, "duration_ms", execTime.Milliseconds())

			telemetry.ActiveWorkers.Dec()
		}
	}
}
