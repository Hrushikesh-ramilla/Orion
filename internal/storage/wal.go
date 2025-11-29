// Package storage implements the persistence layer for the task orchestrator.
// It provides a Write-Ahead Log (WAL) backed by an append-only local file.
package storage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"go-enterprise-scheduler/pkg/models"
)

// WalEntry represents a discrete state change in the system.
type WalEntry struct {
	Type           string        `json:"type"`             // "INGEST", "COMPLETE", "FAIL", "REQUEST"
	Tasks          []models.Task `json:"tasks,omitempty"`  // Used for INGEST
	TaskID         string        `json:"task_id,omitempty"`// Used for COMPLETE/FAIL
	IdempotencyKey string        `json:"idempotency_key,omitempty"` // Used for REQUEST
}

// WAL provides durable persistence for incoming tasks and state changes.
// It acts as an append-only newline-delimited JSON log.
type WAL struct {
	mu                sync.Mutex // Guards append file I/O
	filePath          string
	file              *os.File
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
// This guarantees tasks are durable before they enter the scheduler memory.
func (w *WAL) AppendIngest(tasks []models.Task) error {
	return w.append(WalEntry{Type: "INGEST", Tasks: tasks})
}

// AppendComplete logs that a task finished executing successfully.
func (w *WAL) AppendComplete(taskID string) error {
	return w.append(WalEntry{Type: "COMPLETE", TaskID: taskID})
}

// AppendFail logs that a task failed execution.
func (w *WAL) AppendFail(taskID string) error {
	return w.append(WalEntry{Type: "FAIL", TaskID: taskID})
}

// AppendStart logs that a task started executing.
func (w *WAL) AppendStart(taskID string) error {
	return w.append(WalEntry{Type: "START", TaskID: taskID})
}

// AppendRequest logs the idempotency key for optional best-effort recovery.
func (w *WAL) AppendRequest(key string) error {
	return w.append(WalEntry{Type: "REQUEST", IdempotencyKey: key})
}

// append writes the entry to the file safely and syncs.
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

	// Force flush to disk for strict write-ahead durability.
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("sync wal file: %w", err)
	}
	return nil
}

type RecoveryState struct {
	Entries         []WalEntry
	CompletedTasks  map[string]bool
	FailedTasks     map[string]bool
	InProgressTasks map[string]bool
}

// Recover reads the WAL file line by line and returns the ordered entries.
func (w *WAL) Recover() (*RecoveryState, error) {
	file, err := os.Open(w.filePath)
	if err != nil {
		// Log might not exist yet; clean state.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open wal for recovery: %w", err)
	}

	state := &RecoveryState{
		CompletedTasks:  make(map[string]bool),
		FailedTasks:     make(map[string]bool),
		InProgressTasks: make(map[string]bool),
	}

	reader := bufio.NewReader(file)
	offset := int64(0)

	for {
		line, err := reader.ReadBytes('\n')

		if len(line) > 0 {
			var entry WalEntry
			if jsonErr := json.Unmarshal(line, &entry); jsonErr == nil {
				offset += int64(len(line))
				
				switch entry.Type {
				case "START":
					state.InProgressTasks[entry.TaskID] = true
				case "COMPLETE":
					delete(state.InProgressTasks, entry.TaskID)
					state.CompletedTasks[entry.TaskID] = true
				case "FAIL":
					delete(state.InProgressTasks, entry.TaskID)
					state.FailedTasks[entry.TaskID] = true
				}

				state.Entries = append(state.Entries, entry)
			} else {
				slog.Warn("corrupt wal record detected, breaking", "offset", offset, "error", jsonErr)
				break
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("read wal line: %w", err)
		}
	}

	file.Close()

	if truncErr := os.Truncate(w.filePath, offset); truncErr != nil {
		return nil, fmt.Errorf("truncate damaged wal: %w", truncErr)
	}

	if _, seekErr := w.file.Seek(offset, io.SeekStart); seekErr != nil {
		return nil, fmt.Errorf("seek append handle to new eof: %w", seekErr)
	}

	return state, nil
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
