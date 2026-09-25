package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"talker/internal/hermes"
)

func TestWatchExistingRunNeverAdmitsAndDeduplicatesConcurrentCalls(t *testing.T) {
	f, client := newFake(t)
	f.nondurable = true
	path := filepath.Join(t.TempDir(), "tasks.json")
	m := openTestManager(t, client, path)
	const count = 16
	results := make(chan Task, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			task, err := m.Watch(context.Background(), "run_1")
			if err != nil {
				t.Error(err)
				return
			}
			results <- task
		})
	}
	wg.Wait()
	close(results)
	var id string
	for task := range results {
		if id == "" {
			id = task.ID
		}
		if task.ID != id || task.RunID != "run_1" || task.Status != "running" || task.SessionID != "voice-session" || task.Prompt != "Existing Hermes run run_1" {
			t.Errorf("adopted task = %+v", task)
		}
	}
	if len(m.List()) != 1 || id == "" {
		t.Fatalf("concurrent Watch created %d tasks", len(m.List()))
	}
	saved, err := readLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := saved.Tasks[id]
	if rec.IdempotencyKey != "" || !rec.RetryUntil.IsZero() || rec.ConnectionID != client.Identity() {
		t.Fatalf("adoption must not enter admission recovery: %+v", rec)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		streams := f.streams
		f.mu.Unlock()
		if streams != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	if f.posts != 0 || f.stops != 0 || f.approvals != 0 || f.streams != 1 || len(f.keys) != 0 {
		t.Errorf("Watch remote calls: posts=%d streams=%d stops=%d approvals=%d", f.posts, f.streams, f.stops, f.approvals)
	}
	f.status, f.output = "completed", "The external task is complete."
	f.mu.Unlock()
	f.signal <- "run.completed"
	awaitTask(t, m, id, "completed")
	if len(m.Events()) != 1 || m.Events()[0].TaskID != id {
		t.Fatalf("completion notifications = %+v", m.Events())
	}
}

func TestWatchAlreadyTerminalNotifiesOnceAcrossRestartAndAck(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			f, client := newFake(t)
			f.status, f.output = status, "Existing final result"
			path := filepath.Join(t.TempDir(), "tasks.json")
			m := openTestManager(t, client, path)
			task, err := m.Watch(context.Background(), "run_1")
			if err != nil || task.Status != status || task.Output != f.output {
				t.Fatalf("Watch = %+v, %v", task, err)
			}
			events := m.Events()
			if len(events) != 1 || events[0].Title != "Task "+status || events[0].Body != task.Output {
				t.Fatalf("terminal events = %+v", events)
			}
			m.Close()
			recovered := openTestManager(t, client, path)
			again, err := recovered.Watch(context.Background(), "run_1")
			if err != nil || again.ID != task.ID || len(recovered.List()) != 1 || len(recovered.Events()) != 1 || recovered.Events()[0].ID != events[0].ID {
				t.Fatalf("dedup after restart = %+v, %v, %+v", again, err, recovered.Events())
			}
			if err := recovered.Ack([]string{events[0].ID}); err != nil {
				t.Fatal(err)
			}
			if _, err := recovered.Watch(context.Background(), "run_1"); err != nil || len(recovered.Events()) != 0 {
				t.Fatal("repeated Watch recreated an acknowledged completion")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.posts != 0 || f.streams != 0 || f.stops != 0 {
				t.Fatal("terminal adoption started remote work or a live monitor")
			}
		})
	}
}

func TestWatchRestartsMonitorWithoutAdmissionMetadata(t *testing.T) {
	f, client := newFake(t)
	f.nondurable, f.streamUnavailable = true, true
	path := filepath.Join(t.TempDir(), "tasks.json")
	m := openTestManager(t, client, path)
	task, err := m.Watch(context.Background(), "run_external")
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	f.mu.Lock()
	f.status, f.output = "completed", "Finished while Talker was offline."
	f.mu.Unlock()
	recovered := openTestManager(t, client, path)
	final := awaitTask(t, recovered, task.ID, "completed")
	if final.Output != "Finished while Talker was offline." || len(recovered.Events()) != 1 {
		t.Fatalf("recovered run = %+v, events = %+v", final, recovered.Events())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || len(f.keys) != 0 {
		t.Fatal("restart tried to admit an adopted run")
	}
}

func TestWatchDeduplicatesRunStartedByThisManager(t *testing.T) {
	f, client := newFake(t)
	m := openTestManager(t, client, filepath.Join(t.TempDir(), "tasks.json"))
	started, err := m.Start(context.Background(), "Keep the original prompt", "voice-session")
	if err != nil {
		t.Fatal(err)
	}
	watched, err := m.Watch(context.Background(), started.RunID)
	if err != nil || watched.ID != started.ID || watched.Prompt != started.Prompt || len(m.List()) != 1 {
		t.Fatalf("Watch existing local task = %+v, %v", watched, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatalf("Watch submitted another run: %d admissions", f.posts)
	}
}

func TestWatchDeduplicationIsConnectionScoped(t *testing.T) {
	f1, client1 := newFake(t)
	f2, client2 := newFake(t)
	f1.status, f2.status = "completed", "completed"
	path := filepath.Join(t.TempDir(), "tasks.json")
	first := openTestManager(t, client1, path)
	task1, err := first.Watch(context.Background(), "run_1")
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second := openTestManager(t, client2, path)
	task2, err := second.Watch(context.Background(), "run_1")
	if err != nil || task1.ID == task2.ID || len(second.List()) != 2 || len(second.Events()) != 2 {
		t.Fatalf("different connection was deduplicated: %+v, %v", task2, err)
	}
}

func TestWatchRejectsMissingOrInvalidRunWithoutPersistence(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing", http.StatusNotFound, `{"error":{"code":"run_not_found","message":"Run not found"}}`},
		{"expired", http.StatusGone, `{"error":"Expired"}`},
		{"mismatched", http.StatusOK, `{"run_id":"run_different","status":"running"}`},
		{"invalid-status", http.StatusOK, `{"run_id":"run_1","status":"invented"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/p/voice/v1/runs/run_1" || r.Header.Get("Authorization") != "Bearer secret-key" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			client, err := hermes.New(server.URL+"/p/voice", "secret-key")
			if err != nil {
				t.Fatal(err)
			}
			m := openTestManager(t, client, filepath.Join(t.TempDir(), "tasks.json"))
			task, err := m.Watch(context.Background(), "run_1")
			if err == nil || task.ID != "" || len(m.List()) != 0 || len(m.Events()) != 0 {
				t.Fatalf("invalid Watch = %+v, %v", task, err)
			}
			if test.status == http.StatusNotFound || test.status == http.StatusGone {
				var remote *hermes.HTTPError
				if !strings.Contains(err.Error(), "not found, expired") || !errors.As(err, &remote) || remote.StatusCode != test.status {
					t.Fatalf("unclear or unwrapped missing-run error: %v", err)
				}
			}
		})
	}
}

func TestWatchPersistenceFailureDoesNotStartMonitor(t *testing.T) {
	f, client := newFake(t)
	m := openTestManager(t, client, filepath.Join(t.TempDir(), "tasks.json"))
	m.path = filepath.Join(t.TempDir(), "absent", "tasks.json")
	if _, err := m.Watch(context.Background(), "run_1"); err == nil || len(m.List()) != 0 {
		t.Fatalf("unpersisted Watch accepted: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || f.streams != 0 {
		t.Fatal("unpersisted adoption started monitoring or execution")
	}
}

func TestWatchDisabledClosedAndInvalidIDs(t *testing.T) {
	m := openTestManager(t, nil, filepath.Join(t.TempDir(), "tasks.json"))
	if _, err := m.Watch(context.Background(), "run_1"); !errors.Is(err, hermes.ErrNotConfigured) {
		t.Fatalf("disabled Watch = %v", err)
	}
	f, client := newFake(t)
	configured := openTestManager(t, client, filepath.Join(t.TempDir(), "tasks.json"))
	for _, id := range []string{"", "../run_1", "run_1?query", "run_1\n"} {
		if _, err := configured.Watch(context.Background(), id); err == nil {
			t.Errorf("accepted non-exact run ID %q", id)
		}
	}
	configured.Close()
	if _, err := configured.Watch(context.Background(), "run_1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Watch = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || f.polls != 0 {
		t.Fatal("invalid Watch sent requests")
	}
}

func TestWatchCancellationDoesNotAdoptRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutating request: %s", r.Method)
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := hermes.New(server.URL, "secret-key")
	m := openTestManager(t, client, filepath.Join(t.TempDir(), "tasks.json"))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := m.Watch(ctx, "run_1"); !errors.Is(err, context.DeadlineExceeded) || len(m.List()) != 0 {
		t.Fatalf("cancelled Watch = %v", err)
	}
}

func TestKnownRunWithoutAdmissionFieldsIsValidButPendingIsNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	saved := ledger{Version: 1, Tasks: map[string]record{
		"task_1": {Task: Task{ID: "task_1", RunID: "run_1", Status: "running"}, ConnectionID: "connection"},
	}}
	if _, err := writeLedger(path, saved); err != nil {
		t.Fatal(err)
	}
	if _, err := readLedger(path); err != nil {
		t.Fatalf("known-run ledger rejected: %v", err)
	}
	rec := saved.Tasks["task_1"]
	rec.Task.RunID = ""
	saved.Tasks["task_1"] = rec
	if _, err := writeLedger(path, saved); err != nil {
		t.Fatal(err)
	}
	if _, err := readLedger(path); err == nil {
		data, _ := json.Marshal(saved)
		t.Fatalf("unsafe admission record accepted: %s", data)
	}
}
