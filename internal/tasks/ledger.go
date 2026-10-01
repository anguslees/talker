package tasks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const maxLedgerBytes = 64 << 20

type record struct {
	Task           Task      `json:"task"`
	IdempotencyKey string    `json:"idempotency_key"`
	ConnectionID   string    `json:"connection_id"`
	RetryUntil     time.Time `json:"retry_until"`
	Notified       bool      `json:"notified"`
	ApprovalNotice string    `json:"approval_notice,omitempty"`
}

type ledger struct {
	Version      int                     `json:"version"`
	Tasks        map[string]record       `json:"tasks"`
	Events       []Event                 `json:"events"`
	Conversation *conversation           `json:"conversation,omitempty"`
	Follows      map[string]followRecord `json:"follows,omitempty"`
}

func newID(prefix string) (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(data[:]), nil
}

func readLedger(path string) (ledger, error) {
	empty := ledger{Version: 1, Tasks: make(map[string]record), Events: []Event{}, Follows: make(map[string]followRecord)}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("stat task ledger: %w", err)
	}
	if !info.Mode().IsRegular() {
		return empty, errors.New("task ledger must be a regular file, not a symlink or directory")
	}
	file, err := os.Open(path)
	if err != nil {
		return empty, fmt.Errorf("open task ledger: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxLedgerBytes+1))
	if err != nil {
		return empty, fmt.Errorf("read task ledger: %w", err)
	}
	if len(data) > maxLedgerBytes {
		return empty, errors.New("task ledger exceeds 64 MiB limit")
	}
	var saved ledger
	if err := json.Unmarshal(data, &saved); err != nil {
		return empty, fmt.Errorf("decode task ledger (file was not overwritten): %w", err)
	}
	if saved.Version != 1 || saved.Tasks == nil {
		return empty, errors.New("unsupported or invalid task ledger")
	}
	for id, rec := range saved.Tasks {
		if id == "" || rec.Task.ID != id || rec.Task.Status == "" || rec.ConnectionID == "" ||
			(rec.Task.RunID == "" && (rec.IdempotencyKey == "" || rec.RetryUntil.IsZero())) {
			return empty, errors.New("task ledger contains an invalid task record")
		}
	}
	seen := make(map[string]bool)
	for _, event := range saved.Events {
		if event.ID == "" || seen[event.ID] {
			return empty, errors.New("task ledger contains an invalid notification ID")
		}
		seen[event.ID] = true
	}
	if saved.Events == nil {
		saved.Events = []Event{}
	}
	if c := saved.Conversation; c != nil && (c.ConnectionID == "" || c.Day == "" || (c.SessionID == "" && c.FoundingTask == "")) {
		return empty, errors.New("task ledger contains an invalid conversation")
	}
	for id, rec := range saved.Follows {
		if id == "" || rec.Follow.ID != id || rec.Follow.SessionID == "" || rec.ConnectionID == "" {
			return empty, errors.New("task ledger contains an invalid session follow")
		}
	}
	if saved.Follows == nil {
		saved.Follows = make(map[string]followRecord)
	}
	return saved, nil
}

// writeLedger fsyncs both the replacement and its directory. The boolean tells
// the caller whether rename committed, even if the directory sync then fails.
func writeLedger(path string, state ledger) (bool, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	if len(data) > maxLedgerBytes {
		return false, errors.New("task ledger exceeds 64 MiB limit")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer dir.Close()
	file, err := os.CreateTemp(filepath.Dir(path), ".tasks-*.tmp")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return false, err
	}
	if _, err := file.Write(data); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return false, err
	}
	return true, dir.Sync()
}

func cloneLedger(state ledger) ledger {
	copy := ledger{Version: state.Version, Tasks: make(map[string]record, len(state.Tasks)), Events: append([]Event{}, state.Events...), Follows: make(map[string]followRecord, len(state.Follows))}
	for id, rec := range state.Tasks {
		copy.Tasks[id] = rec
	}
	for id, rec := range state.Follows {
		copy.Follows[id] = rec
	}
	if state.Conversation != nil {
		c := *state.Conversation
		copy.Conversation = &c
	}
	return copy
}
