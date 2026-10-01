// Package tasks keeps one durable ledger and one monitor per Hermes run.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anguslees/talker/internal/hermes"
)

var (
	ErrClosed       = errors.New("task manager is closed")
	ErrTaskNotFound = errors.New("task not found")
	ErrNotAdmitted  = errors.New("task has not yet been admitted by Hermes")
	ErrUnsafeReplay = errors.New("Hermes must support durable run idempotency and status polling to start tasks safely")
)

type Task struct {
	ID           string           `json:"id"`
	RunID        string           `json:"run_id,omitempty"`
	SessionID    string           `json:"session_id,omitempty"`
	Prompt       string           `json:"prompt"`
	Status       string           `json:"status"`
	Output       string           `json:"output,omitempty"`
	Error        string           `json:"error,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
	Approval     *hermes.Approval `json:"approval,omitempty"`
	PendingSteer json.RawMessage  `json:"pending_steer,omitempty"`
	// Conversation records how a task relates to the current conversation:
	// RouteStarted, RouteContinued or RouteSeparate; empty when unrelated.
	Conversation string `json:"conversation,omitempty"`
}

type Event struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type Manager struct {
	mu        sync.Mutex
	client    *hermes.Client
	path      string
	state     ledger
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	workers   map[string]chan struct{}
	followers map[string]bool
	// inline marks tasks whose caller is waiting to receive the outcome
	// directly, so no notification is queued for it.
	inline map[string]bool
	// changed is closed and replaced after every committed ledger change.
	changed   chan struct{}
	now       func() time.Time
	wg        sync.WaitGroup
	lock      *os.File
	closeOnce sync.Once
}

// Open exclusively locks the ledger and resumes observation, not execution, of
// admitted work. Snapshots and notifications are safe for many clients.
func Open(ctx context.Context, client *hermes.Client, path string) (*Manager, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("task ledger path is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
		return nil, fmt.Errorf("create task ledger directory: %w", err)
	}
	lock, err := acquireLedgerLock(absPath + ".lock")
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			lock.Close()
		}
	}()
	state, err := readLedger(absPath)
	if err != nil {
		return nil, err
	}
	if _, err := writeLedger(absPath, state); err != nil {
		return nil, fmt.Errorf("persist task ledger: %w", err)
	}
	root, cancel := context.WithCancel(ctx)
	m := &Manager{
		client: client, path: absPath, state: state, ctx: root, cancel: cancel, lock: lock,
		workers: make(map[string]chan struct{}), followers: make(map[string]bool), inline: make(map[string]bool),
		changed: make(chan struct{}), now: time.Now,
	}
	owned = true
	m.mu.Lock()
	if client.Configured() {
		for id, rec := range state.Tasks {
			if !finished(rec.Task.Status) {
				m.startMonitorLocked(id, nil, false)
			}
		}
		m.startFollowersLocked()
	}
	m.mu.Unlock()
	return m, nil
}

// Start submits prompt to Hermes. An empty sessionID continues the current
// conversation, or runs in a separate session while a turn is running there.
// An explicit sessionID becomes the current conversation, and is refused while
// a turn is running in it.
func (m *Manager) Start(ctx context.Context, prompt, sessionID string) (Task, error) {
	return m.start(ctx, prompt, sessionID, false)
}

func (m *Manager) start(ctx context.Context, prompt, sessionID string, inline bool) (Task, error) {
	if !m.client.Configured() {
		return Task{}, hermes.ErrNotConfigured
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > hermes.MaxPromptBytes || len(sessionID) > 256 || strings.ContainsAny(sessionID, "\r\n\x00") {
		return Task{}, errors.New("task requires a nonempty prompt of at most 64 KiB and a valid session ID")
	}
	m.mu.Lock()
	closed := m.isClosedLocked()
	m.mu.Unlock()
	if closed {
		return Task{}, ErrClosed
	}
	caps, err := m.client.Capabilities(ctx)
	if err != nil {
		return Task{}, err
	}
	retention, err := admissionRetention(caps)
	if err != nil {
		return Task{}, err
	}
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	id, err := newID("task_")
	if err != nil {
		return Task{}, err
	}
	key, err := newID("talker_")
	if err != nil {
		return Task{}, err
	}
	// The target session's newest turn is read before admission, so a turn
	// another client is running there is not interleaved. An explicit session
	// must exist and is continued at its live continuation.
	requested, probed, probe := sessionID, "", (*turnProbe)(nil)
	running := m.runningSessions(ctx)
	if requested != "" {
		p, err := m.probeTurn(ctx, requested)
		if err != nil {
			return Task{}, err
		}
		probed, probe, sessionID = requested, &p, p.page.SessionID
	} else if target := m.conversationToProbe(); target != "" {
		// Unreadable history falls back to what the conversation's follow has seen.
		if p, err := m.probeTurn(ctx, target); err == nil {
			probed, probe = target, &p
		}
	}
	now := m.now().UTC()
	task := Task{ID: id, SessionID: sessionID, Prompt: prompt, Status: "submitting", CreatedAt: now, UpdatedAt: now}
	first := make(chan admissionResult, 1)
	m.mu.Lock()
	if m.isClosedLocked() {
		m.mu.Unlock()
		return Task{}, ErrClosed
	}
	next := cloneLedger(m.state)
	if requested == "" {
		task.SessionID, task.Conversation = m.route(&next, id, probed, probe, running, now)
	} else {
		if err := m.adopt(&next, requested, *probe, running, now); err != nil {
			m.mu.Unlock()
			return Task{}, err
		}
		task.Conversation = RouteContinued
	}
	next.Tasks[id] = record{Task: task, IdempotencyKey: key, ConnectionID: m.client.Identity(), RetryUntil: now.Add(retention)}
	if inline {
		m.inline[id] = true
	}
	if err := m.commitLocked(next); err != nil {
		delete(m.inline, id)
		m.mu.Unlock()
		return task, err
	}
	m.startMonitorLocked(id, first, true)
	m.mu.Unlock()
	// Once persisted, this operation belongs to the manager, not to a browser's
	// connection lifetime. Cancellation of this wait does not cancel remote work.
	select {
	case result := <-first:
		return result.task, result.err
	case <-ctx.Done():
		task, _ := m.Get(id)
		return task, ctx.Err()
	case <-m.ctx.Done():
		task, _ := m.Get(id)
		return task, ErrClosed
	}
}

// Watch adopts an exact existing run. It never submits a prompt or restarts work,
// and repeated calls share the same durable record and monitor for this connection.
func (m *Manager) Watch(ctx context.Context, runID string) (Task, error) {
	if !m.client.Configured() {
		return Task{}, hermes.ErrNotConfigured
	}
	m.mu.Lock()
	closed := m.isClosedLocked()
	m.mu.Unlock()
	if closed {
		return Task{}, ErrClosed
	}
	run, err := m.client.GetRun(ctx, runID)
	if err != nil {
		var remote *hermes.HTTPError
		if errors.As(err, &remote) && (remote.StatusCode == http.StatusNotFound || remote.StatusCode == http.StatusGone) {
			return Task{}, fmt.Errorf("Hermes run not found, expired, or inaccessible to this connection: %w", err)
		}
		return Task{}, fmt.Errorf("watch existing Hermes run: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClosedLocked() {
		return Task{}, ErrClosed
	}
	connectionID := m.client.Identity()
	var rec record
	for _, existing := range m.state.Tasks {
		if existing.ConnectionID == connectionID && existing.Task.RunID == runID {
			rec = existing
			rec.Task = cloneTask(existing.Task)
			break
		}
	}
	if rec.Task.ID == "" {
		id, err := newID("task_")
		if err != nil {
			return Task{}, err
		}
		now := time.Now().UTC()
		rec = record{
			Task:         Task{ID: id, RunID: runID, Prompt: "Existing Hermes run " + runID, CreatedAt: now, UpdatedAt: now},
			ConnectionID: connectionID,
		}
	}
	mergeRun(&rec, run)
	if err := m.storeRecordLocked(rec); err != nil {
		return cloneTask(rec.Task), err
	}
	if !finished(rec.Task.Status) {
		m.startMonitorLocked(rec.Task.ID, nil, false)
	}
	return cloneTask(m.state.Tasks[rec.Task.ID].Task), nil
}

func admissionRetention(caps hermes.Capabilities) (time.Duration, error) {
	idem := caps.Features.RunsIdempotency
	if !caps.Features.RunSubmission || !caps.Features.RunStatus || !idem.Supported || !idem.Durable || idem.RetentionSeconds <= 120 {
		return 0, ErrUnsafeReplay
	}
	// Do not retry at the expiry boundary, where admission and local clocks can differ.
	seconds := min(idem.RetentionSeconds, int64(24*60*60))
	return time.Duration(seconds)*time.Second - time.Minute, nil
}

func (m *Manager) List() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Task, 0, len(m.state.Tasks))
	for _, rec := range m.state.Tasks {
		result = append(result, cloneTask(rec.Task))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func (m *Manager) Get(id string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.state.Tasks[id]
	return cloneTask(rec.Task), ok
}

func (m *Manager) Stop(ctx context.Context, id string) (Task, error) {
	task, err := m.remoteTask(id)
	if err != nil || hermes.IsTerminal(task.Status) {
		return task, err
	}
	run, err := m.client.StopRun(ctx, task.RunID)
	if err != nil {
		return task, err
	}
	if err := m.applyRun(id, run); err != nil {
		return task, err
	}
	m.mu.Lock()
	if !m.isClosedLocked() {
		m.startMonitorLocked(id, nil, false)
		m.wakeLocked(id)
	}
	m.mu.Unlock()
	task, _ = m.Get(id)
	return task, nil
}

func (m *Manager) Steer(ctx context.Context, id, input string) error {
	task, err := m.remoteTask(id)
	if err != nil {
		return err
	}
	return m.client.SteerRun(ctx, task.RunID, input)
}

func (m *Manager) Approve(ctx context.Context, id, requestID, choice string) error {
	task, err := m.remoteTask(id)
	if err != nil {
		return err
	}
	if task.Status != "waiting_for_approval" || task.Approval == nil || task.Approval.RequestID != requestID || !slices.Contains(task.Approval.Choices, choice) {
		return errors.New("approval must match the current pending request and an advertised choice")
	}
	if err := m.client.ApproveRun(ctx, task.RunID, requestID, choice); err != nil {
		return err
	}
	m.mu.Lock()
	m.wakeLocked(id)
	m.mu.Unlock()
	return nil
}

func (m *Manager) remoteTask(id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClosedLocked() {
		return Task{}, ErrClosed
	}
	if !m.client.Configured() {
		return Task{}, hermes.ErrNotConfigured
	}
	rec, ok := m.state.Tasks[id]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	if rec.ConnectionID != m.client.Identity() {
		return cloneTask(rec.Task), errors.New("task belongs to a different Hermes URL/profile or credential")
	}
	if rec.Task.RunID == "" {
		return cloneTask(rec.Task), ErrNotAdmitted
	}
	return cloneTask(rec.Task), nil
}

// Events returns a replayable snapshot. Reading it never acknowledges delivery.
func (m *Manager) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event{}, m.state.Events...)
}

func (m *Manager) Ack(eventIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClosedLocked() {
		return ErrClosed
	}
	ids := make(map[string]bool, len(eventIDs))
	for _, id := range eventIDs {
		ids[id] = true
	}
	next := cloneLedger(m.state)
	next.Events = slices.DeleteFunc(next.Events, func(event Event) bool { return ids[event.ID] })
	if len(next.Events) == len(m.state.Events) {
		return nil
	}
	return m.commitLocked(next)
}

func (m *Manager) AddEvent(title, body string) (Event, error) {
	if strings.TrimSpace(title) == "" || len(title) > 256 || len(body) > hermes.MaxPromptBytes {
		return Event{}, errors.New("event requires a title of at most 256 bytes and a body of at most 64 KiB")
	}
	id, err := newID("event_")
	if err != nil {
		return Event{}, err
	}
	event := Event{ID: id, Title: title, Body: body, CreatedAt: time.Now().UTC()}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClosedLocked() {
		return Event{}, ErrClosed
	}
	next := cloneLedger(m.state)
	next.Events = append(next.Events, event)
	if err := m.commitLocked(next); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()
		m.wg.Wait()
		m.lock.Close()
	})
}

func (m *Manager) isClosedLocked() bool { return m.closed || m.ctx.Err() != nil }

func finished(status string) bool { return hermes.IsTerminal(status) || status == "unknown" }

func cloneTask(task Task) Task {
	if task.Approval != nil {
		copy := *task.Approval
		copy.Choices = append([]string(nil), copy.Choices...)
		task.Approval = &copy
	}
	task.PendingSteer = append(json.RawMessage(nil), task.PendingSteer...)
	return task
}

func (m *Manager) commitLocked(next ledger) error {
	committed, err := writeLedger(m.path, next)
	if committed {
		m.state = next
		close(m.changed)
		m.changed = make(chan struct{})
		m.startFollowersLocked()
	}
	if err != nil {
		return fmt.Errorf("persist task ledger: %w", err)
	}
	return nil
}

func (m *Manager) update(id string, change func(*record)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClosedLocked() {
		return ErrClosed
	}
	original, ok := m.state.Tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	rec := original
	rec.Task = cloneTask(rec.Task)
	change(&rec)
	return m.storeRecordLocked(rec)
}

func (m *Manager) storeRecordLocked(rec record) error {
	id := rec.Task.ID
	original, exists := m.state.Tasks[id]
	next := cloneLedger(m.state)
	var title, body string
	if finished(rec.Task.Status) && !rec.Notified {
		title = "Task " + rec.Task.Status
		body = rec.Task.Output
		if rec.Task.Error != "" {
			body = rec.Task.Error
		}
		if body == "" {
			body = rec.Task.Prompt
		}
		if rec.Task.Status == "unknown" {
			title = "Task needs attention"
		}
		rec.Notified = true
	} else if rec.Task.Status == "waiting_for_approval" && rec.Task.Approval != nil && rec.Task.Approval.RequestID != "" && rec.ApprovalNotice != rec.Task.Approval.RequestID {
		title, body = "Approval required", rec.Task.Approval.Description
		if body == "" {
			body = rec.Task.Approval.Command
		}
		rec.ApprovalNotice = rec.Task.Approval.RequestID
	}
	// A caller waiting inline receives the outcome directly; an approval request
	// is always queued so it reaches the user through the notification channel.
	if title != "" && !(m.inline[id] && finished(rec.Task.Status)) {
		eventID, err := newID("event_")
		if err != nil {
			return err
		}
		next.Events = append(next.Events, Event{ID: eventID, TaskID: id, Title: title, Body: body, CreatedAt: time.Now().UTC()})
	}
	conversationChanged := m.trackConversation(&next, original.Task, rec.Task)
	if exists && !conversationChanged && reflect.DeepEqual(rec, original) {
		return nil
	}
	rec.Task.UpdatedAt = time.Now().UTC()
	next.Tasks[id] = rec
	return m.commitLocked(next)
}

func (m *Manager) applyRun(id string, run hermes.Run) error {
	return m.update(id, func(rec *record) { mergeRun(rec, run) })
}

func mergeRun(rec *record, run hermes.Run) {
	if hermes.IsTerminal(rec.Task.Status) {
		return
	}
	// A status request started before /stop must not undo the local stop acknowledgement.
	if rec.Task.Status == "stopping" && !hermes.IsTerminal(run.Status) {
		run.Status = "stopping"
	}
	if rec.Task.Status == "unknown" {
		rec.Notified = false
	}
	rec.Task.Status, rec.Task.Error = run.Status, run.Error
	if run.SessionID != "" {
		rec.Task.SessionID = run.SessionID
	}
	if run.Output != "" || hermes.IsTerminal(run.Status) {
		rec.Task.Output = run.Output
	}
	rec.Task.Approval = run.Approval
	rec.Task.PendingSteer = run.PendingSteer
}

type admissionResult struct {
	task Task
	err  error
}

func (m *Manager) startMonitorLocked(id string, first chan admissionResult, capabilitiesChecked bool) {
	if _, exists := m.workers[id]; exists {
		return
	}
	wake := make(chan struct{}, 1)
	m.workers[id] = wake
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.workers, id)
			// An explicit Watch can refresh an unknown run while its old monitor
			// is exiting. Preserve observation without ever admitting new work.
			if rec := m.state.Tasks[id]; !m.isClosedLocked() && rec.Task.RunID != "" && !finished(rec.Task.Status) {
				m.startMonitorLocked(id, nil, false)
			}
			m.mu.Unlock()
		}()
		m.monitor(id, wake, first, capabilitiesChecked)
	}()
}

func (m *Manager) wakeLocked(id string) {
	if wake := m.workers[id]; wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (m *Manager) snapshot(id string) record {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.state.Tasks[id]
	rec.Task = cloneTask(rec.Task)
	return rec
}

func (m *Manager) attention(id, message string) error {
	return m.update(id, func(rec *record) {
		if !hermes.IsTerminal(rec.Task.Status) {
			rec.Task.Status, rec.Task.Error, rec.Task.Approval = "unknown", message, nil
		}
	})
}

func (m *Manager) monitor(id string, wake chan struct{}, first chan admissionResult, capabilitiesChecked bool) {
	report := func(err error) {
		if first != nil {
			task, _ := m.Get(id)
			first <- admissionResult{task: task, err: err}
			first = nil
		}
	}
	defer func() { report(ErrClosed) }()
	delay := time.Second
	var admitted *hermes.Admission
	for m.ctx.Err() == nil {
		rec := m.snapshot(id)
		if finished(rec.Task.Status) {
			report(nil)
			return
		}
		if rec.ConnectionID != m.client.Identity() {
			err := m.attention(id, "Hermes URL/profile or credential changed; this task was not resubmitted. Reconcile it on the original server.")
			report(errors.New("task belongs to a different Hermes connection"))
			if err == nil || !wait(m.ctx, delay) {
				return
			}
			continue
		}
		if rec.Task.RunID != "" {
			report(nil)
			break
		}
		if admitted == nil && !time.Now().Before(rec.RetryUntil) {
			err := m.attention(id, "Admission is uncertain and its idempotency retention window expired. This task was not resubmitted; check Hermes before starting replacement work.")
			report(errors.New("task admission requires manual reconciliation"))
			if err == nil || !wait(m.ctx, delay) {
				return
			}
			continue
		}
		if !capabilitiesChecked {
			caps, err := m.client.Capabilities(m.ctx)
			if err == nil {
				_, err = admissionRetention(caps)
				if err != nil {
					if persistErr := m.attention(id, ErrUnsafeReplay.Error()+"; existing admission was not retried"); persistErr == nil {
						report(err)
						return
					}
				}
			}
			if err != nil {
				report(err)
				if !wait(m.ctx, delay) {
					return
				}
				delay = min(delay*2, 30*time.Second)
				continue
			}
			capabilitiesChecked = true
		}
		if admitted == nil {
			result, err := m.client.CreateRun(m.ctx, hermes.RunRequest{Input: rec.Task.Prompt, SessionID: rec.Task.SessionID}, rec.IdempotencyKey)
			if err != nil {
				if m.ctx.Err() != nil {
					return
				}
				permanent := !hermes.Retryable(err)
				persistErr := m.update(id, func(rec *record) {
					rec.Task.Error = err.Error()
					if permanent {
						rec.Task.Status = "failed"
					}
				})
				report(err)
				if permanent && persistErr == nil {
					return
				}
				if !wait(m.ctx, delay) {
					return
				}
				delay = min(delay*2, 30*time.Second)
				continue
			}
			admitted = &result
		}
		// Keep the received ID in this worker until its ledger write succeeds; a
		// storage failure must not turn a successful admission into a new request.
		if err := m.update(id, func(rec *record) {
			rec.Task.RunID, rec.Task.Status, rec.Task.Error = admitted.RunID, "queued", ""
		}); err != nil {
			report(err)
			slog.Error("persist Hermes admission", "task_id", id, "error", err)
			if !wait(m.ctx, delay) {
				return
			}
			continue
		}
		report(nil)
		break
	}
	if m.ctx.Err() != nil {
		return
	}
	m.watchRun(id, wake)
}

func (m *Manager) watchRun(id string, wake chan struct{}) {
	runID := m.snapshot(id).Task.RunID
	if runID == "" {
		return
	}
	streamCtx, cancel := context.WithCancel(m.ctx)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		// Polling owns the result. SSE only accelerates reconciliation, so partial
		// text, replay gaps, duplicate events, and older single-consumer streams
		// cannot produce duplicate notifications or false completion.
		_ = m.client.StreamEvents(streamCtx, runID, "", func(event hermes.RunEvent) error {
			if strings.HasPrefix(event.Event, "run.") || event.Event == "approval.request" || event.Event == "approval.responded" {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
			return nil
		})
		select {
		case wake <- struct{}{}:
		default:
		}
	}()
	defer func() { cancel(); <-streamDone }()
	timer := time.NewTimer(0)
	defer timer.Stop()
	delay, missing := 2*time.Second, 0
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-wake:
		case <-timer.C:
		}
		if finished(m.snapshot(id).Task.Status) {
			return
		}
		run, err := m.client.GetRun(m.ctx, runID)
		if m.ctx.Err() != nil {
			return
		}
		if err != nil {
			var remote *hermes.HTTPError
			if errors.As(err, &remote) && remote.StatusCode == http.StatusNotFound {
				missing++
			} else {
				missing = 0
			}
			if missing >= 3 || (errors.As(err, &remote) && (remote.StatusCode == http.StatusUnauthorized || remote.StatusCode == http.StatusForbidden)) {
				if persistErr := m.attention(id, "Hermes run status is unavailable; the task was not restarted. "+err.Error()); persistErr == nil {
					return
				}
			} else {
				_ = m.update(id, func(rec *record) {
					if !finished(rec.Task.Status) {
						rec.Task.Error = err.Error()
					}
				})
			}
			delay = min(delay*2, 30*time.Second)
		} else {
			missing, delay = 0, 2*time.Second
			if err := m.applyRun(id, run); err != nil {
				slog.Error("persist Hermes status", "task_id", id, "error", err)
			} else if hermes.IsTerminal(run.Status) {
				return
			}
		}
		timer.Reset(delay)
	}
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
