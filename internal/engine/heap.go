package engine

import (
	"container/heap"

	"go-enterprise-scheduler/pkg/models"
)

// TaskHeap implements heap.Interface for priority-based task ordering.
// Lower Priority value = higher execution precedence. This is a Min-Heap.
type TaskHeap []*models.Task

func (h TaskHeap) Len() int { return len(h) }

func (h TaskHeap) Less(i, j int) bool {
	if h[i].Priority == h[j].Priority {
		return h[i].ID < h[j].ID // Deterministic tie-breaking.
	}
	return h[i].Priority < h[j].Priority
}

func (h TaskHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *TaskHeap) Push(x interface{}) {
	*h = append(*h, x.(*models.Task))
}

func (h *TaskHeap) Pop() interface{} {
	old := *h
	n := len(old)
	task := old[n-1]
	old[n-1] = nil // Avoid memory leak.
	*h = old[:n-1]
	return task
}

// PriorityQueue wraps TaskHeap with a clean public API.
type PriorityQueue struct {
	heap TaskHeap
}

func NewPriorityQueue() *PriorityQueue {
	pq := &PriorityQueue{
		heap: make(TaskHeap, 0),
	}
	heap.Init(&pq.heap)
	return pq
}

func (pq *PriorityQueue) Enqueue(task *models.Task) {
	heap.Push(&pq.heap, task)
}

// Dequeue removes and returns the highest-priority task.
// Returns nil if the queue is empty.
func (pq *PriorityQueue) Dequeue() *models.Task {
	if pq.Len() == 0 {
		return nil
	}
	return heap.Pop(&pq.heap).(*models.Task)
}

func (pq *PriorityQueue) Len() int {
	return pq.heap.Len()
}
