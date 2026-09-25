package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"talker/internal/hermes"
)

type fakeHermes struct {
	mu                sync.Mutex
	server            *httptest.Server
	keys              map[string]string
	bodies            map[string]string
	status            string
	output            string
	approval          *hermes.Approval
	posts             int
	polls             int
	streams           int
	stops             int
	steers            int
	approvals         int
	ambiguous         int
	streamUnavailable bool
	nondurable        bool
	admissionHook     func(*http.Request, string)
	signal            chan string
}

func newFake(t *testing.T) (*fakeHermes, *hermes.Client) {
	t.Helper()
	f := &fakeHermes{keys: make(map[string]string), bodies: make(map[string]string), status: "running", signal: make(chan string, 32)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Error("missing API bearer key")
		}
		path := strings.TrimPrefix(r.URL.Path, "/p/voice")
		if path == r.URL.Path {
			t.Errorf("profile prefix missing: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if path == "/v1/capabilities" {
			f.mu.Lock()
			durable := !f.nondurable
			f.mu.Unlock()
			fmt.Fprintf(w, `{"object":"hermes.api_server.capabilities","features":{"run_submission":true,"run_status":true,"run_events_sse":true,"runs_idempotency":{"supported":true,"durable":%t,"retention_seconds":86400}}}`, durable)
			return
		}
		if path == "/v1/runs" && r.Method == http.MethodPost {
			var request hermes.RunRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			body, _ := json.Marshal(request)
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				t.Error("missing idempotency key")
			}
			f.mu.Lock()
			f.posts++
			runID, replay := f.keys[key]
			if !replay {
				runID = fmt.Sprintf("run_%d", len(f.keys)+1)
				f.keys[key], f.bodies[key] = runID, string(body)
			} else if f.bodies[key] != string(body) {
				t.Error("retry changed the admission body")
			}
			ambiguous, hook := f.ambiguous > 0, f.admissionHook
			if ambiguous {
				f.ambiguous--
			}
			f.mu.Unlock()
			if hook != nil {
				hook(r, key)
			}
			if ambiguous {
				// Work was accepted, but its admission response was lost upstream.
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, `{"error":{"message":"upstream response lost"}}`)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(hermes.Admission{RunID: runID, Status: "started", Replayed: replay})
			return
		}
		if strings.HasSuffix(path, "/events") {
			f.mu.Lock()
			f.streams++
			unavailable := f.streamUnavailable
			f.mu.Unlock()
			if unavailable {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"code":"run_not_found"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case event := <-f.signal:
					fmt.Fprintf(w, "data: {\"event\":%q,\"run_id\":\"run_1\"}\n\n", event)
					w.(http.Flusher).Flush()
				}
			}
		}
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) < 3 || parts[0] != "v1" || parts[1] != "runs" {
			t.Errorf("invented route: %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(parts) == 3 && r.Method == http.MethodGet {
			f.polls++
			_ = json.NewEncoder(w).Encode(hermes.Run{Object: "hermes.run", RunID: parts[2], SessionID: "voice-session", Status: f.status, Output: f.output, Approval: f.approval})
			return
		}
		if len(parts) == 4 && r.Method == http.MethodPost {
			switch parts[3] {
			case "stop":
				f.stops++
				f.status = "cancelled"
				_ = json.NewEncoder(w).Encode(hermes.Run{RunID: parts[2], Status: "stopping"})
			case "steer":
				f.steers++
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["input"] != "use citations" {
					t.Error("incorrect steer body")
				}
				fmt.Fprintf(w, `{"run_id":%q,"accepted":true}`, parts[2])
			case "approval":
				f.approvals++
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["request_id"] != "approval-1" || body["choice"] != "deny" {
					t.Error("incorrect explicit approval")
				}
				f.status, f.approval = "running", nil
				fmt.Fprintf(w, `{"run_id":%q,"resolved":1}`, parts[2])
			default:
				t.Errorf("invented control route: %s", path)
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(f.server.Close)
	client, err := hermes.New(f.server.URL+"/p/voice", "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	return f, client
}

func openTestManager(t *testing.T, client *hermes.Client, path string) *Manager {
	t.Helper()
	m, err := Open(context.Background(), client, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func awaitTask(t *testing.T, m *Manager, id, status string) Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := m.Get(id)
		if ok && task.Status == status {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := m.Get(id)
	t.Fatalf("task did not reach %q: %+v", status, task)
	return Task{}
}

func TestAdmissionIsDurableBeforePOSTAndCompletionDeduplicates(t *testing.T) {
	f, client := newFake(t)
	path := filepath.Join(t.TempDir(), "private", "tasks.json")
	f.admissionHook = func(_ *http.Request, key string) {
		state, err := readLedger(path)
		if err != nil || len(state.Tasks) != 1 {
			t.Errorf("admission not persisted first: %+v %v", state, err)
		}
		for _, rec := range state.Tasks {
			if rec.IdempotencyKey != key || rec.Task.Status != "submitting" {
				t.Errorf("incorrect pre-admission record: %+v", rec)
			}
		}
		info, _ := os.Stat(path)
		if info == nil || info.Mode().Perm() != 0o600 {
			t.Error("ledger is not private")
		}
	}
	m := openTestManager(t, client, path)
	task, err := m.Start(context.Background(), "research the topic", "voice-session")
	if err != nil || task.RunID == "" {
		t.Fatalf("Start = %+v, %v", task, err)
	}
	awaitTask(t, m, task.ID, "running")
	f.mu.Lock()
	f.status, f.output = "completed", "A researched answer."
	f.mu.Unlock()
	f.signal <- "run.completed"
	f.signal <- "run.completed"
	final := awaitTask(t, m, task.ID, "completed")
	if final.Output != "A researched answer." || len(m.Events()) != 1 {
		t.Fatalf("final = %+v, events = %+v", final, m.Events())
	}
	first, second := m.Events(), m.Events()
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Events destructively consumed notifications")
	}
	if err := m.applyRun(task.ID, hermes.Run{Status: "completed", Output: final.Output}); err != nil || len(m.Events()) != 1 {
		t.Fatal("terminal reconciliation duplicated completion")
	}
	f.mu.Lock()
	if f.posts != 1 || f.streams > 1 || f.stops != 0 || f.approvals != 0 {
		t.Errorf("unexpected automatic actions: %+v", []int{f.posts, f.streams, f.stops, f.approvals})
	}
	f.mu.Unlock()
	if err := m.Ack([]string{first[0].ID, "already-absent"}); err != nil {
		t.Fatal(err)
	}
	m.Close()
	reopened := openTestManager(t, client, path)
	if len(reopened.Events()) != 0 {
		t.Fatal("acknowledged completion reappeared after restart")
	}
	if got, _ := reopened.Get(task.ID); got.Output != final.Output {
		t.Fatal("final result was not persisted")
	}
}

func TestUncertainAdmissionRecoversWithSameKey(t *testing.T) {
	f, client := newFake(t)
	f.ambiguous = 1
	path := filepath.Join(t.TempDir(), "tasks.json")
	m := openTestManager(t, client, path)
	task, err := m.Start(context.Background(), "one costly operation", "")
	if err == nil || task.ID == "" || task.RunID != "" || task.Status != "submitting" {
		t.Fatalf("ambiguous admission = %+v, %v", task, err)
	}
	m.Close()
	f.mu.Lock()
	f.status, f.output = "completed", "only executed once"
	f.mu.Unlock()
	recovered := openTestManager(t, client, path)
	final := awaitTask(t, recovered, task.ID, "completed")
	if final.Output != "only executed once" || len(recovered.Events()) != 1 {
		t.Fatalf("recovery = %+v, %+v", final, recovered.Events())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 2 || len(f.keys) != 1 {
		t.Fatalf("posts=%d, unique admissions=%d", f.posts, len(f.keys))
	}
}

func TestResumeMonitorWithoutResubmittingAndPollingFallback(t *testing.T) {
	f, client := newFake(t)
	f.streamUnavailable = true
	path := filepath.Join(t.TempDir(), "tasks.json")
	m := openTestManager(t, client, path)
	task, err := m.Start(context.Background(), "continue observing", "")
	if err != nil {
		t.Fatal(err)
	}
	awaitTask(t, m, task.ID, "running")
	m.Close()
	f.mu.Lock()
	f.status, f.output = "interrupted", "partial result"
	f.mu.Unlock()
	recovered := openTestManager(t, client, path)
	awaitTask(t, recovered, task.ID, "interrupted")
	if len(recovered.Events()) != 1 {
		t.Fatal("missing interruption notification")
	}
	recovered.Close()
	again := openTestManager(t, client, path)
	if got, _ := again.Get(task.ID); got.Status != "interrupted" {
		t.Fatal("interrupted work was restarted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || f.stops != 0 {
		t.Fatalf("restart performed remote actions: posts=%d stops=%d", f.posts, f.stops)
	}
}

func TestCallerDisconnectDoesNotCancelAdmittedWork(t *testing.T) {
	f, client := newFake(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.admissionHook = func(r *http.Request, _ string) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	m := openTestManager(t, client, filepath.Join(t.TempDir(), "tasks.json"))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan admissionResult, 1)
	go func() {
		task, err := m.Start(ctx, "work independently", "")
		result <- admissionResult{task, err}
	}()
	<-entered
	cancel()
	got := <-result
	if !errors.Is(got.err, context.Canceled) || got.task.ID == "" {
		t.Fatalf("cancelled wait = %+v", got)
	}
	close(release)
	awaitTask(t, m, got.task.ID, "running")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || f.stops != 0 {
		t.Fatal("disconnect duplicated or stopped the operation")
	}
}

func TestExplicitStopSteerAndApproval(t *testing.T) {
	f, client := newFake(t)
	f.status = "waiting_for_approval"
	f.approval = &hermes.Approval{RequestID: "approval-1", Command: "delete files", Description: "Confirm deletion", Choices: []string{"once", "deny"}}
	m := openTestManager(t, client, filepath.Join(t.TempDir(), "tasks.json"))
	task, err := m.Start(context.Background(), "a gated operation", "")
	if err != nil {
		t.Fatal(err)
	}
	awaitTask(t, m, task.ID, "waiting_for_approval")
	if err := m.Approve(context.Background(), task.ID, "wrong-request", "deny"); err == nil {
		t.Fatal("accepted stale approval")
	}
	if err := m.Approve(context.Background(), task.ID, "approval-1", "always"); err == nil {
		t.Fatal("accepted unadvertised approval scope")
	}
	snapshot, _ := m.Get(task.ID)
	snapshot.Approval.Choices[0] = "always"
	if current, _ := m.Get(task.ID); current.Approval.Choices[0] != "once" {
		t.Fatal("Get exposed mutable internal state")
	}
	if err := m.Approve(context.Background(), task.ID, "approval-1", "deny"); err != nil {
		t.Fatal(err)
	}
	awaitTask(t, m, task.ID, "running")
	if err := m.Steer(context.Background(), task.ID, "use citations"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stop(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	awaitTask(t, m, task.ID, "cancelled")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.approvals != 1 || f.steers != 1 || f.stops != 1 {
		t.Fatalf("explicit action counts: approvals=%d steers=%d stops=%d", f.approvals, f.steers, f.stops)
	}
}

func TestExpiredOrChangedScopeNeverRetriesAdmission(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed=%t", changed), func(t *testing.T) {
			f, client := newFake(t)
			path := filepath.Join(t.TempDir(), "tasks.json")
			now := time.Now().UTC()
			rec := record{Task: Task{ID: "task_saved", Prompt: "uncertain work", Status: "submitting", CreatedAt: now, UpdatedAt: now}, IdempotencyKey: "same-key", ConnectionID: client.Identity(), RetryUntil: now.Add(-time.Hour)}
			if changed {
				rec.ConnectionID, rec.RetryUntil = "old-connection", now.Add(time.Hour)
			}
			_, err := writeLedger(path, ledger{Version: 1, Tasks: map[string]record{rec.Task.ID: rec}, Events: []Event{}})
			if err != nil {
				t.Fatal(err)
			}
			m := openTestManager(t, client, path)
			awaitTask(t, m, rec.Task.ID, "unknown")
			if len(m.Events()) != 1 {
				t.Fatal("missing unsafe recovery notice")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.posts != 0 {
				t.Fatal("unsafe resubmission occurred")
			}
		})
	}
}

func TestUnconfiguredAndNondurableCreateNoFakeTasks(t *testing.T) {
	disabled, _ := hermes.New("", "")
	m := openTestManager(t, disabled, filepath.Join(t.TempDir(), "tasks.json"))
	if _, err := m.Start(context.Background(), "do work", ""); !errors.Is(err, hermes.ErrNotConfigured) || len(m.List()) != 0 {
		t.Fatalf("disabled Start: %v, %+v", err, m.List())
	}
	f, client := newFake(t)
	f.nondurable = true
	unsafe := openTestManager(t, client, filepath.Join(t.TempDir(), "tasks.json"))
	if _, err := unsafe.Start(context.Background(), "do work", ""); !errors.Is(err, ErrUnsafeReplay) || len(unsafe.List()) != 0 {
		t.Fatalf("nondurable Start: %v, %+v", err, unsafe.List())
	}
}

func TestEventsPersistAndAckIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	m := openTestManager(t, nil, path)
	event, err := m.AddEvent("Calendar alert", "Meeting in five minutes")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := m.Events()
	snapshot[0].Body = "mutated"
	if m.Events()[0].Body != event.Body {
		t.Fatal("Events exposed internal slice")
	}
	m.Close()
	m = openTestManager(t, nil, path)
	if !reflect.DeepEqual(m.Events(), []Event{event}) {
		t.Fatalf("recovered events = %+v", m.Events())
	}
	if err := m.Ack([]string{event.ID, event.ID}); err != nil {
		t.Fatal(err)
	}
	if err := m.Ack([]string{event.ID}); err != nil || len(m.Events()) != 0 {
		t.Fatal("ack was not idempotent")
	}
	m.Close()
	if _, err := m.AddEvent("after close", ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("AddEvent after Close: %v", err)
	}
}

func TestCorruptLedgerIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	data := []byte("{broken JSON")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), nil, path); err == nil {
		t.Fatal("corrupt ledger accepted")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(data) {
		t.Fatal("corrupt ledger overwritten")
	}
}

func TestPersistenceFailurePreventsAdmission(t *testing.T) {
	f, client := newFake(t)
	m := openTestManager(t, client, filepath.Join(t.TempDir(), "tasks.json"))
	m.path = filepath.Join(t.TempDir(), "absent", "tasks.json")
	if _, err := m.Start(context.Background(), "must not be sent", ""); err == nil {
		t.Fatal("failed storage accepted Start")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || len(m.List()) != 0 {
		t.Fatal("posted work before durable ledger write")
	}
}

func TestExclusiveLedgerOwnershipAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	owner := openTestManager(t, nil, path)
	if other, err := Open(context.Background(), nil, path); !errors.Is(err, ErrLedgerLocked) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("duplicate owner = %v", err)
	}
	info, err := os.Stat(path + ".lock")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock permissions = %v, %v", info, err)
	}
	owner.Close()
	next := openTestManager(t, nil, path)
	if _, err := next.AddEvent("Next owner", "Lock released on Close"); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	if other, err := Open(context.Background(), nil, path); !errors.Is(err, ErrLedgerLocked) {
		if other != nil {
			other.Close()
		}
		t.Fatal("repeated Close released another manager's ownership")
	}
}

func TestConcurrentTasksAndEvents(t *testing.T) {
	f, client := newFake(t)
	f.status, f.output, f.streamUnavailable = "completed", "complete", true
	path := filepath.Join(t.TempDir(), "tasks.json")
	m := openTestManager(t, client, path)
	const count = 12
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			if _, err := m.Start(context.Background(), fmt.Sprintf("task %d", i), ""); err != nil {
				t.Error(err)
			}
			if _, err := m.AddEvent("external", fmt.Sprintf("event %d", i)); err != nil {
				t.Error(err)
			}
			_ = m.List()
			_ = m.Events()
		})
	}
	wg.Wait()
	for _, task := range m.List() {
		awaitTask(t, m, task.ID, "completed")
	}
	if len(m.List()) != count || len(m.Events()) != 2*count {
		t.Fatalf("tasks=%d events=%d", len(m.List()), len(m.Events()))
	}
	m.Close()
	recovered := openTestManager(t, client, path)
	if len(recovered.List()) != count || len(recovered.Events()) != 2*count {
		t.Fatal("concurrent durable writes lost data")
	}
}

func TestLedgerLockCrossProcess(t *testing.T) {
	if path := os.Getenv("TALKER_TEST_LOCK_PATH"); path != "" {
		manager, err := Open(context.Background(), nil, path)
		if err == nil {
			manager.Close()
			t.Fatal("child process acquired a live owner's lock")
		}
		if !errors.Is(err, ErrLedgerLocked) {
			t.Fatalf("child lock error = %v", err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "tasks.json")
	_ = openTestManager(t, nil, path)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLedgerLockCrossProcess$")
	command.Env = append(os.Environ(), "TALKER_TEST_LOCK_PATH="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cross-process lock check: %v\n%s", err, output)
	}
}
