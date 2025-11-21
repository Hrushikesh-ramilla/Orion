package models

import "time"

// Task represents a single unit of work in the orchestrator.
type Task struct {
	ID           string   `json:"id"`
	Payload      string   `json:"payload"`
	Priority     int      `json:"priority"`
	Dependencies []string `json:"dependencies"`
	Status       string   `json:"status"`

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
