package engine

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"sync/atomic"
	"time"

	"go-enterprise-scheduler/internal/storage"
	"go-enterprise-scheduler/pkg/models"
	"go-enterprise-scheduler/pkg/telemetry"
)

// Atomic metrics — updated inside runLoop, read lock-free from any goroutine.
type atomicMetrics struct {
	pending   atomic.Int64
	running   atomic.Int64
	completed atomic.Int64
	failed    atomic.Int64
	retried   atomic.Int64
}

type ingestReq struct {
	tasks []models.Task
	resp  chan error
}

type Scheduler struct {
	tasks      map[string]*models.Task
	inDegree   map[string]int
	dependents map[string][]string
	readyQueue *PriorityQueue
	wal        *storage.WAL
	metrics    atomicMetrics

	ingestChan   chan ingestReq
	completeChan chan string
	failChan     chan string
	popReqChan   chan chan *models.Task
	retryChan    chan string

	replayStartChan    chan string
	replayCompleteChan chan string
	replayFailChan     chan string
	requeueOrphansChan chan struct{ orphans map[string]bool; done chan struct{} }

	readyChan     chan *models.Task
	queueSize     atomic.Int64
}

func NewScheduler(wal *storage.WAL) *Scheduler {
	return &Scheduler{
		tasks:              make(map[string]*models.Task),
		inDegree:           make(map[string]int),
		dependents:         make(map[string][]string),
		readyQueue:         NewPriorityQueue(),
		wal:                wal,
		ingestChan:         make(chan ingestReq),
		completeChan:       make(chan string),
		failChan:           make(chan string),
		popReqChan:         make(chan chan *models.Task),
		retryChan:          make(chan string),
		replayStartChan:    make(chan string),
		replayCompleteChan: make(chan string),
		replayFailChan:     make(chan string),
		requeueOrphansChan: make(chan struct{ orphans map[string]bool; done chan struct{} }),
		readyChan:          make(chan *models.Task),
	}
}

func (s *Scheduler) Start(ctx context.Context) {
	go s.runLoop(ctx)
}

func (s *Scheduler) StartDispatch(ctx context.Context) {
	go s.pushLoop(ctx)
}

func (s *Scheduler) ReadyTasks() <-chan *models.Task {
	return s.readyChan
}

func (s *Scheduler) runLoop(ctx context.Context) {
	for {
		var activePopReq chan chan *models.Task
		if s.readyQueue.Len() > 0 {
			activePopReq = s.popReqChan
		}

		select {
		case req := <-activePopReq:
			var task *models.Task
			for s.readyQueue.Len() > 0 {
				t := s.readyQueue.Dequeue()
				s.queueSize.Add(-1)
				telemetry.CurrentQueueSize.Dec()
				if t.Status == models.StatusReady {
					task = t
					break
				}
			}
			if task != nil {
				s.metrics.pending.Add(-1)
				s.metrics.running.Add(1)
				task.Status = models.StatusRunning
			}
			req <- task

		case req := <-s.ingestChan:
			s.handleIngest(req)

		case taskID := <-s.completeChan:
			s.handleComplete(taskID)

		case taskID := <-s.failChan:
			s.handleFailure(taskID)

		case taskID := <-s.replayStartChan:
			s.handleReplayStart(taskID)

		case taskID := <-s.replayCompleteChan:
			s.handleReplayComplete(taskID)

		case taskID := <-s.replayFailChan:
			s.handleReplayFail(taskID)

		case req := <-s.requeueOrphansChan:
			s.handleRequeueOrphans(req.orphans)
			close(req.done)

		case taskID := <-s.retryChan:
			s.handleRetryEnqueue(taskID)

		case <-ctx.Done():
			return
		}
	}
}

func (s *Scheduler) handleIngest(req ingestReq) {
	log.Println("SCHED: ingest")
	for i := range req.tasks {
		t := &req.tasks[i]
		if _, exists := s.tasks[t.ID]; exists {
			req.resp <- fmt.Errorf("scheduler: duplicate task ID %q", t.ID)
			return
		}
	}

	for i := range req.tasks {
		t := &req.tasks[i]
		t.Status = models.StatusPending
		s.metrics.pending.Add(1)
		if t.MaxRetries == 0 {
			t.MaxRetries = 3 // sensible default
		}
		s.tasks[t.ID] = t
		s.inDegree[t.ID] = len(t.Dependencies)

		for _, depID := range t.Dependencies {
			s.dependents[depID] = append(s.dependents[depID], t.ID)
		}
	}

	telemetry.TasksIngestedTotal.Add(float64(len(req.tasks)))

	for i := range req.tasks {
		t := &req.tasks[i]
		if s.inDegree[t.ID] == 0 {
			t.Status = models.StatusReady
			// pending→ready is still "pending" in metrics (both count as pending)
			s.readyQueue.Enqueue(t)
			s.queueSize.Add(1)
			telemetry.CurrentQueueSize.Inc()
			log.Println("SCHED: task ready")
			slog.Info("task ready", "task_id", t.ID, "reason", "no_dependencies")
		}
	}
	req.resp <- nil
}

func (s *Scheduler) handleComplete(taskID string) {
	log.Println("SCHED: complete")
	task, exists := s.tasks[taskID]
	if !exists {
		slog.Warn("attempted to complete unknown task", "task_id", taskID)
		return
	}

	s.metrics.running.Add(-1)
	s.metrics.completed.Add(1)
	task.Status = models.StatusCompleted
	telemetry.TasksCompletedTotal.Inc()
	slog.Info("task completed", "task_id", taskID)

	for _, depID := range s.dependents[taskID] {
		s.inDegree[depID]--
		if s.inDegree[depID] == 0 {
			depTask := s.tasks[depID]
			depTask.Status = models.StatusReady
			s.readyQueue.Enqueue(depTask)
			s.queueSize.Add(1)
			telemetry.CurrentQueueSize.Inc()
			log.Println("SCHED: task ready")
			slog.Info("task ready", "task_id", depID, "reason", "deps_satisfied")
		}
	}
}

func (s *Scheduler) handleFailure(taskID string) {
	task, exists := s.tasks[taskID]
	if !exists {
		return
	}

	if task.RetryCount < task.MaxRetries {
		task.RetryCount++
		telemetry.TasksRetriedTotal.Inc()
		delay := time.Duration(1<<task.RetryCount) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		slog.Warn("task failed, retrying", "task_id", taskID, "delay", delay.String(), "attempt", task.RetryCount, "max_retries", task.MaxRetries)

		s.metrics.running.Add(-1)
		s.metrics.pending.Add(1)
		s.metrics.retried.Add(1)
		task.Status = models.StatusPending

		time.AfterFunc(delay, func() {
			s.retryChan <- taskID
		})
	} else {
		// Permanent failure — cascade to all downstream dependents.
		s.propagateFailure(taskID)
	}
}

// propagateFailure marks a task and all its transitive dependents as failed
// using BFS. A visited set prevents infinite loops and re-processing.
func (s *Scheduler) propagateFailure(rootID string) {
	queue := []string{rootID}
	visited := make(map[string]bool)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current] {
			continue
		}
		visited[current] = true

		task, exists := s.tasks[current]
		if !exists {
			continue
		}
		if task.Status == models.StatusCompleted || task.Status == models.StatusFailed {
			continue
		}

		// Transition from whatever state to failed.
		switch task.Status {
		case models.StatusPending, models.StatusReady:
			s.metrics.pending.Add(-1)
		case models.StatusRunning:
			s.metrics.running.Add(-1)
		}
		s.metrics.failed.Add(1)
		task.Status = models.StatusFailed
		task.Cancelled.Store(1) // Atomic signal for workers
		telemetry.TasksFailedTotal.Inc()
		if err := s.wal.AppendFail(current); err != nil {
			slog.Error("failed to persist cascade failure to WAL", "task_id", current, "error", err)
		}
		slog.Error("task failed permanently", "task_id", current)

		// Enqueue all direct dependents for cascading failure.
		for _, depID := range s.dependents[current] {
			if !visited[depID] {
				queue = append(queue, depID)
			}
		}
	}
}

func (s *Scheduler) handleRetryEnqueue(taskID string) {
	task, exists := s.tasks[taskID]
	if !exists {
		return
	}
	if task.Status == models.StatusPending {
		task.Status = models.StatusReady
		s.readyQueue.Enqueue(task)
		s.queueSize.Add(1)
		telemetry.CurrentQueueSize.Inc()
		slog.Info("task ready for retry", "task_id", taskID)
	}
}

// Metrics reads atomic counters — zero blocking, safe from any goroutine.
func (s *Scheduler) Metrics() (pending, running, completed, failed, retried int) {
	return int(s.metrics.pending.Load()),
		int(s.metrics.running.Load()),
		int(s.metrics.completed.Load()),
		int(s.metrics.failed.Load()),
		int(s.metrics.retried.Load())
}

func (s *Scheduler) pushLoop(ctx context.Context) {
	defer close(s.readyChan)
	for {
		req := make(chan *models.Task, 1)
		select {
		case s.popReqChan <- req:
		case <-ctx.Done():
			return
		}

		select {
		case task := <-req:
			if task != nil {
				select {
				case s.readyChan <- task:
				case <-ctx.Done():
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Scheduler) Ingest(tasks []models.Task) error {
	req := ingestReq{
		tasks: tasks,
		resp:  make(chan error, 1),
	}
	s.ingestChan <- req
	return <-req.resp
}

func (s *Scheduler) Complete(taskID string) {
	s.completeChan <- taskID
}

func (s *Scheduler) ReplayStart(taskID string) {
	s.replayStartChan <- taskID
}

func (s *Scheduler) handleReplayStart(taskID string) {
	task, exists := s.tasks[taskID]
	if !exists { return }
	s.metrics.pending.Add(-1)
	s.metrics.running.Add(1)
	task.Status = models.StatusRunning
}

func (s *Scheduler) RequeueOrphans(inProgress map[string]bool) {
	req := make(chan struct{})
	payload := struct{ orphans map[string]bool; done chan struct{} }{inProgress, req}
	s.requeueOrphansChan <- payload
	<-req
}

func (s *Scheduler) handleRequeueOrphans(orphans map[string]bool) {
	for taskID := range orphans {
		if task, exists := s.tasks[taskID]; exists {
			// Orphans were running at crash time; metrics already show
			// them as running from handleReplayStart, move to pending.
			s.metrics.running.Add(-1)
			s.metrics.pending.Add(1)
			task.Status = models.StatusReady
			s.readyQueue.Enqueue(task)
			s.queueSize.Add(1)
			telemetry.CurrentQueueSize.Inc()
			slog.Info("requeued orphan task", "task_id", taskID)
		}
	}
}

func (s *Scheduler) ReplayComplete(taskID string) {
	s.replayCompleteChan <- taskID
}

func (s *Scheduler) handleReplayComplete(taskID string) {
	task, exists := s.tasks[taskID]
	if !exists { return }
	s.metrics.running.Add(-1)
	s.metrics.completed.Add(1)
	task.Status = models.StatusCompleted

	for _, depID := range s.dependents[taskID] {
		s.inDegree[depID]--
		if s.inDegree[depID] == 0 {
			depTask := s.tasks[depID]
			depTask.Status = models.StatusReady
			s.readyQueue.Enqueue(depTask)
			s.queueSize.Add(1)
			telemetry.CurrentQueueSize.Inc()
		}
	}
}

func (s *Scheduler) Fail(taskID string) {
	s.failChan <- taskID
}

func (s *Scheduler) ReplayFail(taskID string) {
	s.replayFailChan <- taskID
}

func (s *Scheduler) handleReplayFail(taskID string) {
	task, exists := s.tasks[taskID]
	if !exists { return }

	task.RetryCount++
	if task.RetryCount >= task.MaxRetries {
		s.metrics.running.Add(-1)
		s.metrics.failed.Add(1)
		task.Status = models.StatusFailed
	} else {
		s.metrics.running.Add(-1)
		s.metrics.pending.Add(1)
		s.metrics.retried.Add(1)
		task.Status = models.StatusReady
		s.readyQueue.Enqueue(task)
		s.queueSize.Add(1)
		telemetry.CurrentQueueSize.Inc()
	}
}

func (s *Scheduler) QueueSize() int {
	return int(s.queueSize.Load())
}


