// Package engine implements the core scheduling and execution components
// of the Distributed Task Orchestrator. This file provides a Min-Heap
// priority queue backed by Go's container/heap interface.
package engine

import (
	"container/heap"

	"go-enterprise-scheduler/pkg/models"
)

// -----------------------------------------------------------------
// TaskHeap implements heap.Interface for priority-based task ordering.
// Lower Priority value = higher execution precedence. This is a Min-Heap.
// -----------------------------------------------------------------

// TaskHeap is the underlying slice type for the priority queue.
// It satisfies sort.Interface and heap.Interface so that Go's container/heap
// package can manage the heap invariant for us.
type TaskHeap []*models.Task

// Len returns the number of tasks in the heap.
func (h TaskHeap) Len() int { return len(h) }

// Less reports whether task i should be dequeued before task j.
// We compare by Priority (lower = higher precedence). Ties are broken
// lexicographically by Task ID for deterministic ordering.
func (h TaskHeap) Less(i, j int) bool {
	if h[i].Priority == h[j].Priority {
		return h[i].ID < h[j].ID // Deterministic tie-breaking.
	}
	return h[i].Priority < h[j].Priority
}

// Swap exchanges two elements in the heap.
func (h TaskHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

// Push adds a task to the heap. Called by heap.Push; do not call directly.
func (h *TaskHeap) Push(x interface{}) {
	*h = append(*h, x.(*models.Task))
}

// Pop removes and returns the highest-priority task (lowest Priority value).
// Called by heap.Pop; do not call directly.
func (h *TaskHeap) Pop() interface{} {
	old := *h
	n := len(old)
	task := old[n-1]
	old[n-1] = nil // Avoid memory leak.
	*h = old[:n-1]
	return task
}

// -----------------------------------------------------------------
// PriorityQueue wraps TaskHeap with a clean public API.
// -----------------------------------------------------------------

// PriorityQueue provides a thread-unsafe min-heap priority queue for tasks.
// Callers are responsible for external synchronization when using this
// from multiple goroutines.
type PriorityQueue struct {
	heap TaskHeap
}

// NewPriorityQueue creates an empty priority queue, ready for use.
func NewPriorityQueue() *PriorityQueue {
	pq := &PriorityQueue{
		heap: make(TaskHeap, 0),
	}
	heap.Init(&pq.heap)
	return pq
}

// Enqueue inserts a task into the priority queue.
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

// Len returns the number of tasks currently in the queue.
func (pq *PriorityQueue) Len() int {
	return pq.heap.Len()
}
