package models

// Task represents a single unit of work in the orchestrator.
// Tasks form a Directed Acyclic Graph (DAG) through their Dependencies field.
type Task struct {
	// ID is a unique identifier for this task.
	ID string `json:"id"`

	// Payload contains the task's workload description.
	Payload string `json:"payload"`

	// Priority determines execution order. Lower = higher priority.
	Priority int `json:"priority"`

	// Dependencies is a list of Task IDs that must complete first.
	Dependencies []string `json:"dependencies"`

	// Status tracks the current lifecycle state.
	Status string `json:"status"`
}

// Task status constants.
const (
	StatusPending   = "pending"
	StatusReady     = "ready"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)
