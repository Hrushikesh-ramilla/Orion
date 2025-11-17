package engine

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"sync"

	"go-enterprise-scheduler/pkg/models"
)

type ingestReq struct {
	tasks []models.Task
	resp  chan error
}

type Scheduler struct {
	mu         sync.Mutex
	tasks      map[string]*models.Task
	inDegree   map[string]int
	dependents map[string][]string
	readyQueue *PriorityQueue
	taskChan   chan *models.Task
	ingestChan   chan ingestReq
	completeChan chan string
	popReqChan   chan chan *models.Task
}

func NewScheduler() *Scheduler {
	return &Scheduler{
		tasks:        make(map[string]*models.Task),
		inDegree:     make(map[string]int),
		dependents:   make(map[string][]string),
		readyQueue:   NewPriorityQueue(),
		taskChan:     make(chan *models.Task),
		ingestChan:   make(chan ingestReq),
		completeChan: make(chan string),
		popReqChan:   make(chan chan *models.Task),
	}
}

func (s *Scheduler) Start(ctx context.Context) {
	go s.runLoop(ctx)
	go s.pushLoop(ctx)
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
			task.Status = models.StatusRunning
			req <- task
		case req := <-s.ingestChan:
			s.handleIngest(req)
		case taskID := <-s.completeChan:
			s.handleComplete(taskID)
		case <-ctx.Done():
			return
		}
	}
}

func (s *Scheduler) pushLoop(ctx context.Context) {
	defer close(s.taskChan)
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
				case s.taskChan <- task:
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

func (s *Scheduler) Ingest(tasks []models.Task) error {
	req := ingestReq{tasks: tasks, resp: make(chan error, 1)}
	s.ingestChan <- req
	return <-req.resp
}

func (s *Scheduler) Complete(taskID string) {
	s.completeChan <- taskID
}

func (s *Scheduler) ReadyTasks() <-chan *models.Task {
	return s.taskChan
}

func (s *Scheduler) Metrics() (pending, running, completed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
