package models

// Task represents a single unit of work in the orchestrator.
// Tasks form a Directed Acyclic Graph (DAG) through their Dependencies field.
// The scheduler uses Priority to determine execution order among ready tasks.
type Task struct {
	// ID is a unique, human-readable identifier for this task (e.g., "build-frontend").
	ID string `json:"id"`

	// Payload contains the task's workload description or command to execute.
	Payload string `json:"payload"`

	// Priority determines execution order when multiple tasks are ready.
	// Lower integer value = higher priority (e.g., 0 is highest priority).
	Priority int `json:"priority"`

	// Dependencies is a list of Task IDs that must complete before this task can run.
	// An empty slice means the task has no prerequisites and is immediately eligible.
	Dependencies []string `json:"dependencies"`

	// Status tracks the current lifecycle state of the task.
	// Valid values: "pending", "ready", "running", "completed", "failed".
	// OWNED BY runLoop -- do NOT read from worker goroutines.
	Status string `json:"status"`
}

// Task status constants provide a canonical set of lifecycle states.
const (
	StatusPending   = "pending"
	StatusReady     = "ready"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)
