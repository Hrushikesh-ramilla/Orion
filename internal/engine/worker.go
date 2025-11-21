package engine

import (
	"context"
	"log"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"go-enterprise-scheduler/internal/storage"
)

const DefaultWorkerCount = 4

type Dispatcher struct {
	scheduler   *Scheduler
	wal         *storage.WAL
	workerCount int
	wg          sync.WaitGroup
}

func NewDispatcher(scheduler *Scheduler, wal *storage.WAL, workerCount int) *Dispatcher {
	return &Dispatcher{
		scheduler:   scheduler,
		wal:         wal,
		workerCount: workerCount,
	}
}

func (d *Dispatcher) Start(ctx context.Context) {
	for i := 1; i <= d.workerCount; i++ {
		d.wg.Add(1)
		go d.worker(ctx, i)
	}
	slog.Info("dispatcher started", "workerCount", d.workerCount)
}

func (d *Dispatcher) Wait() {
	d.wg.Wait()
	slog.Info("all workers stopped")
}

func (d *Dispatcher) worker(ctx context.Context, id int) {
	defer d.wg.Done()
	slog.Info("worker online", "worker_id", id)
	for {
		select {
		case <-ctx.Done():
			slog.Info("worker offline (context cancelled)", "worker_id", id)
			return
		case task, ok := <-d.scheduler.ReadyTasks():
			if !ok {
				slog.Info("worker offline (channel closed)", "worker_id", id)
				return
			}
			log.Println("WORKER: picked task", task.ID)
			task.StartTime = time.Now()
			slog.Info("executing task", "worker_id", id, "task_id", task.ID, "payload", task.Payload)

			execTime := time.Duration(50+rand.Intn(150)) * time.Millisecond
			if task.Payload == "sleep" {
				execTime = 2000 * time.Millisecond
			}

			select {
			case <-time.After(execTime):
				if task.Payload == "fail" {
					slog.Warn("simulated failure", "worker_id", id, "task_id", task.ID)
					if err := d.wal.AppendFail(task.ID); err != nil {
						slog.Error("failed to write fail event to WAL", "worker_id", id, "error", err)
						continue
					}
					d.scheduler.Fail(task.ID)
					continue
				}
			case <-ctx.Done():
				slog.Warn("interrupted during execution", "worker_id", id, "task_id", task.ID)
				return
			}

			task.EndTime = time.Now()
			if err := d.wal.AppendComplete(task.ID); err != nil {
				slog.Error("failed to write completion to WAL", "worker_id", id, "error", err)
			}
			d.scheduler.Complete(task.ID)
			log.Println("WORKER: finished task", task.ID)
			slog.Info("finished task", "worker_id", id, "task_id", task.ID, "duration_ms", execTime.Milliseconds())
		}
	}
}
