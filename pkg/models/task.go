package models

import (
	"sync/atomic"
	"time"
)

// Task represents a single unit of work in the orchestrator.
// Tasks form a Directed Acyclic Graph (DAG) through their Dependencies field.
// The scheduler uses Priority to determine execution order among ready tasks.
type Task struct {
	ID           string   `json:"id"`
	Payload      string   `json:"payload"`
	Priority     int      `json:"priority"`
	Dependencies []string `json:"dependencies"`

	// Status tracks the current lifecycle state of the task.
	// OWNED BY runLoop -- do NOT read from worker goroutines.
	Status string `json:"status"`

	// Cancelled is an atomic flag set by the scheduler during failure cascade.
	// Workers read this instead of Status to avoid a data race.
	Cancelled atomic.Int32 `json:"-"`

	// Retry tracking
	RetryCount int `json:"retry_count"`
	MaxRetries int `json:"max_retries"`

	// Execution tracking
	StartTime time.Time `json:"start_time,omitempty"`
	EndTime   time.Time `json:"end_time,omitempty"`
}

const (
	StatusPending   = "pending"
	StatusReady     = "ready"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)
