package engine

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"go-enterprise-scheduler/pkg/models"
)

type ingestReq struct {
	tasks []models.Task
	resp  chan error
}

type Scheduler struct {
	tasks      map[string]*models.Task
	inDegree   map[string]int
	dependents map[string][]string
	readyQueue *PriorityQueue

	ingestChan   chan ingestReq
	completeChan chan string
	failChan     chan string
	popReqChan   chan chan *models.Task
	retryChan    chan string
	readyChan    chan *models.Task
}

func NewScheduler() *Scheduler {
	return &Scheduler{
		tasks:        make(map[string]*models.Task),
		inDegree:     make(map[string]int),
		dependents:   make(map[string][]string),
		readyQueue:   NewPriorityQueue(),
		ingestChan:   make(chan ingestReq),
		completeChan: make(chan string),
		failChan:     make(chan string),
		popReqChan:   make(chan chan *models.Task),
		retryChan:    make(chan string),
		readyChan:    make(chan *models.Task),
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
			task := s.readyQueue.Dequeue()
			if task != nil {
				task.Status = models.StatusRunning
			}
			req <- task
		case req := <-s.ingestChan:
			s.handleIngest(req)
		case taskID := <-s.completeChan:
			s.handleComplete(taskID)
		case taskID := <-s.failChan:
			s.handleFailure(taskID)
		case taskID := <-s.retryChan:
			s.handleRetryEnqueue(taskID)
		case <-ctx.Done():
			return
		}
	}
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
		if t.MaxRetries == 0 {
			t.MaxRetries = 3
		}
		s.tasks[t.ID] = t
		s.inDegree[t.ID] = len(t.Dependencies)
		for _, depID := range t.Dependencies {
			s.dependents[depID] = append(s.dependents[depID], t.ID)
		}
	}
	for i := range req.tasks {
		t := &req.tasks[i]
		if s.inDegree[t.ID] == 0 {
			t.Status = models.StatusReady
			s.readyQueue.Enqueue(t)
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
	task.Status = models.StatusCompleted
	slog.Info("task completed", "task_id", taskID)
	for _, depID := range s.dependents[taskID] {
		s.inDegree[depID]--
		if s.inDegree[depID] == 0 {
			depTask := s.tasks[depID]
			depTask.Status = models.StatusReady
			s.readyQueue.Enqueue(depTask)
			log.Println("SCHED: task ready")
			slog.Info("task ready", "task_id", depID, "reason", "deps_satisfied")
		}
	}
}

func (s *Scheduler) handleFailure(taskID string) {
	task, exists := s.tasks[taskID]
	if !exists { return }

	if task.RetryCount < task.MaxRetries {
		task.RetryCount++
		delay := time.Duration(1<<task.RetryCount) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		slog.Warn("task failed, retrying", "task_id", taskID, "delay", delay.String(), "attempt", task.RetryCount)
		task.Status = models.StatusPending
		time.AfterFunc(delay, func() {
			s.retryChan <- taskID
		})
	} else {
		task.Status = models.StatusFailed
		slog.Error("task failed permanently", "task_id", taskID)
	}
}

func (s *Scheduler) handleRetryEnqueue(taskID string) {
	task, exists := s.tasks[taskID]
	if !exists { return }
	if task.Status == models.StatusPending {
		task.Status = models.StatusReady
		s.readyQueue.Enqueue(task)
		slog.Info("task ready for retry", "task_id", taskID)
	}
}

func (s *Scheduler) Ingest(tasks []models.Task) error {
	req := ingestReq{tasks: tasks, resp: make(chan error, 1)}
	s.ingestChan <- req
	return <-req.resp
}

func (s *Scheduler) Complete(taskID string) { s.completeChan <- taskID }
func (s *Scheduler) Fail(taskID string)     { s.failChan <- taskID }

func (s *Scheduler) Metrics() (pending, running, completed int) {
	for _, t := range s.tasks {
		switch t.Status {
		case models.StatusPending, models.StatusReady:
			pending++
		case models.StatusRunning:
			running++
		case models.StatusCompleted:
			completed++
		}
	}
	return
}
