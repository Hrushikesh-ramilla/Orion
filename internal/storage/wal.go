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

type WalEntry struct {
	Type           string        `json:"type"`
	Tasks          []models.Task `json:"tasks,omitempty"`
	TaskID         string        `json:"task_id,omitempty"`
	IdempotencyKey string        `json:"idempotency_key,omitempty"`
}

type WAL struct {
	mu       sync.Mutex
	filePath string
	file     *os.File
}

func NewWAL(filePath string) (*WAL, error) {
	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("wal: failed to open log: %w", err)
	}
	return &WAL{filePath: filePath, file: file}, nil
}

func (w *WAL) AppendIngest(tasks []models.Task) error {
	return w.append(WalEntry{Type: "INGEST", Tasks: tasks})
}

func (w *WAL) AppendComplete(taskID string) error {
	return w.append(WalEntry{Type: "COMPLETE", TaskID: taskID})
}

func (w *WAL) AppendFail(taskID string) error {
	return w.append(WalEntry{Type: "FAIL", TaskID: taskID})
}

func (w *WAL) AppendStart(taskID string) error {
	return w.append(WalEntry{Type: "START", TaskID: taskID})
}

func (w *WAL) AppendRequest(key string) error {
	return w.append(WalEntry{Type: "REQUEST", IdempotencyKey: key})
}

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

func (w *WAL) Recover() (*RecoveryState, error) {
	file, err := os.Open(w.filePath)
	if err != nil {
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
		if err == io.EOF { break }
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

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}
