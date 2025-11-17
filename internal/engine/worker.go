package engine

import (
	"context"
	"log"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

const DefaultWorkerCount = 4

type Dispatcher struct {
	scheduler   *Scheduler
	workerCount int
	wg          sync.WaitGroup
}

func NewDispatcher(scheduler *Scheduler, workerCount int) *Dispatcher {
	return &Dispatcher{
		scheduler:   scheduler,
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
			slog.Info("executing task", "worker_id", id, "task_id", task.ID, "payload", task.Payload)
			execTime := time.Duration(50+rand.Intn(150)) * time.Millisecond
			time.Sleep(execTime)
			d.scheduler.Complete(task.ID)
			log.Println("WORKER: finished task", task.ID)
			slog.Info("finished task", "worker_id", id, "task_id", task.ID, "duration_ms", execTime.Milliseconds())
		}
	}
}
