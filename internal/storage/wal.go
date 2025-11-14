package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"go-enterprise-scheduler/pkg/models"
)

// WalEntry represents a discrete state change in the system.
type WalEntry struct {
	Type   string        `json:"type"`
	Tasks  []models.Task `json:"tasks,omitempty"`
	TaskID string        `json:"task_id,omitempty"`
}

// WAL provides durable persistence for incoming tasks and state changes.
// It acts as an append-only newline-delimited JSON log.
type WAL struct {
	mu       sync.Mutex
	filePath string
	file     *os.File
}

// NewWAL creates or opens a Write-Ahead Log instance.
func NewWAL(filePath string) (*WAL, error) {
	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("wal: failed to open log: %w", err)
	}

	return &WAL{
		filePath: filePath,
		file:     file,
	}, nil
}

// AppendIngest writes a batch of ingested tasks to the WAL.
func (w *WAL) AppendIngest(tasks []models.Task) error {
	return w.append(WalEntry{Type: "INGEST", Tasks: tasks})
}

// AppendComplete logs that a task finished executing successfully.
func (w *WAL) AppendComplete(taskID string) error {
	return w.append(WalEntry{Type: "COMPLETE", TaskID: taskID})
}

// append writes the entry to the file safely.
func (w *WAL) append(entry WalEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal wal entry: %w", err)
	}
	data = append(data, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.file.Write(data); err != nil {
		return fmt.Errorf("write wal record: %w", err)
	}
	return nil
}

// Close safely shuts down the WAL file descriptor.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}
