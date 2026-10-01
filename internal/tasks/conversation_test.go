package tasks

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anguslees/talker/internal/hermes"
)

type testClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// openConversationManager opens a manager whose follows poll quickly and whose
// clock the test controls. It must run before any follow exists.
func openConversationManager(t *testing.T, f *fakeHermes, path string) (*Manager, *testClock) {
	t.Helper()
	saved := followDelayFor
	followDelayFor = func(time.Duration, int) time.Duration { return 5 * time.Millisecond }
	t.Cleanup(func() { followDelayFor = saved })
	client, err := f.clientFor()
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(context.Background(), client, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	clock := &testClock{}
	m.mu.Lock()
	m.now = clock.now
	m.mu.Unlock()
	return m, clock
}

func (f *fakeHermes) setRun(status, output string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.output = status, output
}

func (f *fakeHermes) runSession(runID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runSessions[runID]
}

func (f *fakeHermes) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts
}

func await(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// awaitCaughtUp waits until every follow has read all of the fake's messages.
func awaitCaughtUp(t *testing.T, m *Manager, f *fakeHermes) {
	t.Helper()
	await(t, "follows to catch up", func() bool {
		f.mu.Lock()
		newest := f.messageID
		f.mu.Unlock()
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, rec := range m.state.Follows {
			if rec.LastMessageID < newest {
				return false
			}
		}
		return true
	})
}

func eventsFor(m *Manager, sessionID string) []Event {
	var result []Event
	for _, e := range m.Events() {
		if e.SessionID == sessionID {
			result = append(result, e)
		}
	}
	return result
}

func TestConversationContinuesOneSessionWithOneRunningTurn(t *testing.T) {
	f, _ := newFake(t)
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	first, err := m.Start(context.Background(), "investigate the cron failure", "")
	if err != nil || first.Conversation != RouteStarted || first.SessionID != "" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if f.runSession(first.RunID) != first.RunID {
		t.Fatal("the founding run must let Hermes create its session")
	}
	awaitTask(t, m, first.ID, "running")
	conv, ok := m.CurrentConversation()
	if !ok || conv.SessionID != first.RunID || conv.ActiveTask != first.ID {
		t.Fatalf("conversation = %+v, %t", conv, ok)
	}
	// The conversation is busy, so unrelated work must not share its session.
	second, err := m.Start(context.Background(), "what is the weather", "")
	if err != nil || second.Conversation != RouteSeparate || f.runSession(second.RunID) != second.RunID {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if _, err := m.Start(context.Background(), "fix harder", first.RunID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("explicit busy session: %v", err)
	}
	if f.postCount() != 2 {
		t.Fatal("a refused request reached Hermes")
	}
	f.setRun("completed", "done")
	awaitTask(t, m, first.ID, "completed")
	awaitTask(t, m, second.ID, "completed")
	third, err := m.Start(context.Background(), "fix harder", "")
	if err != nil || third.Conversation != RouteContinued || third.SessionID != first.RunID || f.runSession(third.RunID) != first.RunID {
		t.Fatalf("third = %+v, %v", third, err)
	}
}

func TestConversationEndsAtDayBoundaryAndOnRequest(t *testing.T) {
	f, _ := newFake(t)
	f.status, f.output = "completed", "done"
	path := filepath.Join(t.TempDir(), "tasks.json")
	m, clock := openConversationManager(t, f, path)
	first, _ := m.Start(context.Background(), "first", "")
	awaitTask(t, m, first.ID, "completed")
	next, _ := m.Start(context.Background(), "second", "")
	if next.Conversation != RouteContinued {
		t.Fatalf("same day = %+v", next)
	}
	awaitTask(t, m, next.ID, "completed")
	clock.advance(24 * time.Hour)
	if _, ok := m.CurrentConversation(); ok {
		t.Fatal("conversation outlived its day")
	}
	tomorrow, _ := m.Start(context.Background(), "tomorrow", "")
	if tomorrow.Conversation != RouteStarted || tomorrow.SessionID != "" {
		t.Fatalf("next day = %+v", tomorrow)
	}
	awaitTask(t, m, tomorrow.ID, "completed")
	ended, err := m.NewConversation()
	if err != nil || ended != tomorrow.RunID {
		t.Fatalf("NewConversation = %q, %v", ended, err)
	}
	fresh, _ := m.Start(context.Background(), "fresh", "")
	if fresh.Conversation != RouteStarted {
		t.Fatalf("after NewConversation = %+v", fresh)
	}
	awaitTask(t, m, fresh.ID, "completed")
	m.Close()
	reopened, _ := openConversationManager(t, f, path)
	reopened.mu.Lock()
	reopened.now = clock.now
	reopened.mu.Unlock()
	if c, ok := reopened.CurrentConversation(); !ok || c.SessionID != fresh.RunID {
		t.Fatalf("conversation lost on restart: %+v", c)
	}
}

func TestExplicitSessionBecomesTheConversationAtItsLiveContinuation(t *testing.T) {
	f, _ := newFake(t)
	f.addTurnAt("webui_old", "old question", "old answer", time.Now().Add(-time.Hour))
	f.session("webui_old").continuation = "webui_tip"
	f.addTurnAt("webui_tip", "later question", "later answer", time.Now().Add(-time.Hour))
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	if _, err := m.Start(context.Background(), "hello", "missing_session"); err == nil || !strings.Contains(err.Error(), "not found") || f.postCount() != 0 {
		t.Fatalf("unknown session: %v", err)
	}
	task, err := m.Start(context.Background(), "continue the triage", "webui_old")
	if err != nil || task.SessionID != "webui_tip" || task.Conversation != RouteContinued || f.runSession(task.RunID) != "webui_tip" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if c, ok := m.CurrentConversation(); !ok || c.SessionID != "webui_tip" {
		t.Fatalf("conversation = %+v", c)
	}
	f.setRun("completed", "triaged")
	awaitTask(t, m, task.ID, "completed")
	next, _ := m.Start(context.Background(), "and the next one", "")
	if next.SessionID != "webui_tip" || next.Conversation != RouteContinued {
		t.Fatalf("follow-up = %+v", next)
	}
	awaitTask(t, m, next.ID, "completed")
	awaitCaughtUp(t, m, f)
	if got := eventsFor(m, "webui_tip"); len(got) != 0 {
		t.Fatalf("history was announced: %+v", got)
	}
}

func TestTurnFromAnotherClientIsNotInterleaved(t *testing.T) {
	f, _ := newFake(t)
	f.addTurnAt("webui", "fix harder", "", time.Now())
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	if _, err := m.Start(context.Background(), "status?", "webui"); !errors.Is(err, ErrSessionBusy) || f.postCount() != 0 {
		t.Fatalf("running web UI turn: %v", err)
	}
	f.session("webui").messages = nil
	f.addTurnAt("webui", "fix harder", "", time.Now().Add(-time.Hour))
	f.setRun("completed", "status")
	task, err := m.Start(context.Background(), "status?", "webui")
	if err != nil || task.SessionID != "webui" {
		t.Fatalf("an unanswered message an hour old must not block: %+v, %v", task, err)
	}
	awaitTask(t, m, task.ID, "completed")
	f.addTurn("webui", "and now this", "")
	awaitCaughtUp(t, m, f)
	routed, err := m.Start(context.Background(), "unrelated", "")
	if err != nil || routed.Conversation != RouteSeparateExternal {
		t.Fatalf("conversation with a web UI turn running = %+v, %v", routed, err)
	}
}

func TestConversationAnnouncesRepliesFromOtherClientsOnce(t *testing.T) {
	f, _ := newFake(t)
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	task, err := m.Start(context.Background(), "investigate the cron failure", "")
	if err != nil {
		t.Fatal(err)
	}
	f.addTurn(task.RunID, "investigate the cron failure", "a prompt tweak")
	f.setRun("completed", "a prompt tweak")
	awaitTask(t, m, task.ID, "completed")
	awaitCaughtUp(t, m, f)
	if len(m.Events()) != 1 || m.Events()[0].TaskID != task.ID {
		t.Fatalf("Talker's own turn must be announced once, by its task: %+v", m.Events())
	}
	f.addTurn(task.RunID, "I reran it and it failed the same way. Fix harder.", "")
	f.addTurn(task.RunID, "", "The real fix, tested.")
	awaitCaughtUp(t, m, f)
	replies := eventsFor(m, task.RunID)
	if len(replies) != 1 || replies[0].Body != "The real fix, tested." || replies[0].TaskID != "" {
		t.Fatalf("web UI reply = %+v", replies)
	}
}

func TestExplicitFollowAcrossCompressionUntilBoundary(t *testing.T) {
	f, _ := newFake(t)
	f.addTurnAt("20260928_triage", "triage the PRs", "three need review", time.Now().Add(-time.Hour))
	path := filepath.Join(t.TempDir(), "tasks.json")
	m, _ := openConversationManager(t, f, path)
	followed, err := m.Follow(context.Background(), "20260928_triage")
	if err != nil || followed.SessionID != "20260928_triage" || !followed.Explicit {
		t.Fatalf("Follow = %+v, %v", followed, err)
	}
	if again, _ := m.Follow(context.Background(), "20260928_triage"); again.ID != followed.ID || len(m.Follows()) != 1 {
		t.Fatal("repeated follow duplicated the session")
	}
	if _, err := m.Follow(context.Background(), "missing_session"); err == nil {
		t.Fatal("followed a missing session")
	}
	awaitCaughtUp(t, m, f)
	if len(m.Events()) != 0 {
		t.Fatal("existing replies were announced")
	}
	// A restarted Talker keeps following, and compression moves the session.
	m.Close()
	m, _ = openConversationManager(t, f, path)
	s := f.session("20260928_triage")
	f.mu.Lock()
	s.continuation, s.endedAt, s.endReason = "20260928_triage_2", 1, "compression"
	f.mu.Unlock()
	f.addTurn("20260928_triage_2", "merge the first one", "Merged.")
	await(t, "the compressed reply", func() bool { return len(eventsFor(m, "20260928_triage_2")) == 1 })
	follows := m.Follows()
	if len(follows) != 1 || follows[0].SessionID != "20260928_triage_2" || follows[0].Origin != "20260928_triage" {
		t.Fatalf("follow after compression = %+v", follows)
	}
	if found, err := m.Unfollow("20260928_triage"); !found || err != nil || len(m.Follows()) != 0 {
		t.Fatalf("Unfollow by original ID = %t, %v", found, err)
	}
	if _, err := m.Follow(context.Background(), "20260928_triage_2"); err != nil {
		t.Fatal(err)
	}
	s = f.session("20260928_triage_2")
	f.mu.Lock()
	s.endedAt, s.endReason = 2, "new_session"
	f.mu.Unlock()
	await(t, "a reset to end the follow", func() bool { return len(m.Follows()) == 0 })
}

func TestAskReturnsInlineOrBecomesATask(t *testing.T) {
	f, _ := newFake(t)
	f.status, f.output = "completed", "Canberra."
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	quick, done, err := m.Ask(context.Background(), "capital of Australia?", "", time.Second)
	if err != nil || !done || quick.Output != "Canberra." || quick.Conversation != RouteStarted {
		t.Fatalf("quick = %+v, %t, %v", quick, done, err)
	}
	if len(m.Events()) != 0 {
		t.Fatal("an answer returned inline was also queued")
	}
	f.setRun("running", "")
	slow, done, err := m.Ask(context.Background(), "research the history", "", 300*time.Millisecond)
	if err != nil || done || slow.RunID == "" || slow.SessionID != quick.RunID {
		t.Fatalf("slow = %+v, %t, %v", slow, done, err)
	}
	f.setRun("completed", "A long history.")
	awaitTask(t, m, slow.ID, "completed")
	await(t, "the background answer", func() bool { return len(m.Events()) == 1 })
	if m.Events()[0].TaskID != slow.ID {
		t.Fatalf("events = %+v", m.Events())
	}
	if f.postCount() != 2 {
		t.Fatalf("posts = %d", f.postCount())
	}
}

// addSteps stores n intermediate tool-calling rows of a running turn.
func (f *fakeHermes) addSteps(sessionID string, n int) {
	s := f.session(sessionID)
	f.mu.Lock()
	defer f.mu.Unlock()
	for range n {
		f.messageID++
		s.messages = append(s.messages, map[string]any{"id": f.messageID, "role": "tool", "content": "{}", "timestamp": float64(time.Now().UnixMilli()) / 1000})
	}
}

func (f *fakeHermes) setSessionsDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionsDown = down
}

func (f *fakeHermes) rotate(from, to string) {
	s := f.session(from)
	f.session(to)
	f.mu.Lock()
	defer f.mu.Unlock()
	s.continuation, s.endedAt, s.endReason = to, 1, "compression"
}

func TestConversationSurvivesItsSessionAppearingLate(t *testing.T) {
	f, _ := newFake(t)
	f.lazySessions = true
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	first, err := m.Start(context.Background(), "long research", "")
	if err != nil {
		t.Fatal(err)
	}
	awaitTask(t, m, first.ID, "running")
	// Many follow polls pass before Hermes starts the turn and creates its session.
	time.Sleep(100 * time.Millisecond)
	if c, ok := m.CurrentConversation(); !ok || c.SessionID != first.RunID || len(m.Follows()) != 1 {
		t.Fatalf("conversation before its session exists = %+v, %t, follows %+v", c, ok, m.Follows())
	}
	f.addTurn(first.RunID, "long research", "found it")
	f.setRun("completed", "found it")
	awaitTask(t, m, first.ID, "completed")
	awaitCaughtUp(t, m, f)
	next, err := m.Start(context.Background(), "follow up", "")
	if err != nil || next.Conversation != RouteContinued || next.SessionID != first.RunID {
		t.Fatalf("follow-up = %+v, %v", next, err)
	}
	if got := eventsFor(m, first.RunID); len(got) != 0 {
		t.Fatalf("Talker's own turn announced by the follow: %+v", got)
	}
}

func TestCompactionCopiesAreNotAnnouncedAgain(t *testing.T) {
	f, _ := newFake(t)
	f.addTurnAt("webui", "q1", "a1", time.Now().Add(-time.Hour))
	f.addTurnAt("webui", "q2", "a2", time.Now().Add(-time.Hour))
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	if _, err := m.Follow(context.Background(), "webui"); err != nil {
		t.Fatal(err)
	}
	f.addTurn("webui", "q3", "a3")
	await(t, "the new reply", func() bool { return len(eventsFor(m, "webui")) == 1 })
	f.compact("webui", 12)
	awaitCaughtUp(t, m, f)
	if got := eventsFor(m, "webui"); len(got) != 1 {
		t.Fatalf("compaction copies announced: %+v", got)
	}
	// A reply compacted before the follow read it is still announced once.
	f.setSessionsDown(true)
	time.Sleep(20 * time.Millisecond)
	f.addTurn("webui", "q4", "a4")
	f.compact("webui", 8)
	f.setSessionsDown(false)
	awaitCaughtUp(t, m, f)
	got := eventsFor(m, "webui")
	if len(got) != 2 || got[1].Body != "a4" {
		t.Fatalf("after compacting an unread reply: %+v", got)
	}
}

func TestLongTurnFromAnotherClientBlocksTheSession(t *testing.T) {
	f, _ := newFake(t)
	f.addTurnAt("webui", "earlier", "answered", time.Now().Add(-time.Hour))
	f.setRun("completed", "ok")
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	adopted, err := m.Start(context.Background(), "status?", "webui")
	if err != nil {
		t.Fatal(err)
	}
	awaitTask(t, m, adopted.ID, "completed")
	f.addTurn("webui", "fix harder", "")
	f.addSteps("webui", 3*followPage)
	if _, err := m.Start(context.Background(), "status?", "webui"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("explicit request during a long web UI turn: %v", err)
	}
	// The conversation is checked afresh, not only by what its follow has seen.
	f.setSessionsDown(false)
	routed, err := m.Start(context.Background(), "unrelated", "")
	if err != nil || routed.Conversation != RouteSeparateExternal {
		t.Fatalf("conversation request during a long web UI turn = %+v, %v", routed, err)
	}
}

func TestAskQueuesApprovalRequests(t *testing.T) {
	f, _ := newFake(t)
	f.status = "waiting_for_approval"
	f.approval = &hermes.Approval{RequestID: "approval-1", Command: "restart gateway", Description: "Restart the gateway", Choices: []string{"once", "deny"}}
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	task, done, err := m.Ask(context.Background(), "restart it", "", time.Second)
	if err != nil || !done || task.Status != "waiting_for_approval" {
		t.Fatalf("Ask = %+v, %t, %v", task, done, err)
	}
	events := m.Events()
	if len(events) != 1 || events[0].Title != "Approval required" || events[0].TaskID != task.ID {
		t.Fatalf("approval notification = %+v", events)
	}
}

func TestAskWithoutADurableRecordReturnsNoTask(t *testing.T) {
	f, _ := newFake(t)
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	m.mu.Lock()
	m.path = filepath.Join(t.TempDir(), "absent", "tasks.json")
	m.mu.Unlock()
	task, done, err := m.Ask(context.Background(), "capital of Australia?", "", time.Second)
	if err == nil || done || task.ID != "" || f.postCount() != 0 {
		t.Fatalf("Ask = %+v, %t, %v", task, done, err)
	}
}

func TestRotatedSessionStaysBusyWhileItsTaskRuns(t *testing.T) {
	f, _ := newFake(t)
	f.status, f.output = "completed", "done"
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	first, _ := m.Start(context.Background(), "first", "")
	awaitTask(t, m, first.ID, "completed")
	f.setRun("running", "")
	second, err := m.Start(context.Background(), "second", "")
	if err != nil || second.SessionID != first.RunID {
		t.Fatalf("second = %+v, %v", second, err)
	}
	awaitTask(t, m, second.ID, "running")
	// Compression rotates the session mid-turn; Hermes keeps reporting the run
	// under the session it was admitted to.
	f.rotate(first.RunID, "rotated")
	for _, name := range []string{"rotated", first.RunID} {
		if _, err := m.Start(context.Background(), "third", name); !errors.Is(err, ErrSessionBusy) {
			t.Fatalf("explicit %s during the rotated turn: %v", name, err)
		}
	}
	if routed, _ := m.Start(context.Background(), "unrelated", ""); routed.Conversation != RouteSeparate {
		t.Fatalf("conversation request during the rotated turn = %+v", routed)
	}
}

func TestRotatedFollowIsNotDuplicated(t *testing.T) {
	f, _ := newFake(t)
	f.addTurnAt("triage", "triage the PRs", "three need review", time.Now().Add(-time.Hour))
	m, _ := openConversationManager(t, f, filepath.Join(t.TempDir(), "tasks.json"))
	if _, err := m.Follow(context.Background(), "triage"); err != nil {
		t.Fatal(err)
	}
	awaitCaughtUp(t, m, f)
	f.setSessionsDown(true)
	time.Sleep(20 * time.Millisecond)
	// The continuation starts with a copy of the compacted turn.
	f.rotate("triage", "triage_2")
	f.addTurnAt("triage_2", "triage the PRs", "three need review", time.Now().Add(-time.Hour))
	f.setSessionsDown(false)
	f.setRun("completed", "merged")
	task, err := m.Start(context.Background(), "merge the first", "triage")
	if err != nil || task.SessionID != "triage_2" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	follows := m.Follows()
	if len(follows) != 1 || follows[0].SessionID != "triage_2" || follows[0].Origin != "triage" || !follows[0].Conversation || !follows[0].Explicit {
		t.Fatalf("follows = %+v", follows)
	}
	awaitTask(t, m, task.ID, "completed")
	f.addTurn("triage_2", "and the second?", "Merged too.")
	await(t, "the reply", func() bool { return len(eventsFor(m, "triage_2")) > 0 })
	awaitCaughtUp(t, m, f)
	if got := eventsFor(m, "triage_2"); len(got) != 1 {
		t.Fatalf("reply announced %d times", len(got))
	}
}

func TestReadPositionIsSavedWithTheNextChange(t *testing.T) {
	f, _ := newFake(t)
	f.addTurnAt("webui", "q1", "a1", time.Now().Add(-time.Hour))
	path := filepath.Join(t.TempDir(), "tasks.json")
	m, _ := openConversationManager(t, f, path)
	followed, err := m.Follow(context.Background(), "webui")
	if err != nil {
		t.Fatal(err)
	}
	saved := func() int64 {
		t.Helper()
		state, err := readLedger(path)
		if err != nil {
			t.Fatal(err)
		}
		return state.Follows[followed.ID].LastMessageID
	}
	before := saved()
	f.addTurn("webui", "q2", "")
	f.addSteps("webui", 5)
	awaitCaughtUp(t, m, f)
	if saved() != before {
		t.Fatal("a poll without announcements rewrote the ledger")
	}
	f.addTurn("webui", "", "a2")
	await(t, "the reply", func() bool { return len(eventsFor(m, "webui")) == 1 })
	f.mu.Lock()
	newest := f.messageID
	f.mu.Unlock()
	if saved() != newest {
		t.Fatal("the read position was not saved with the announcement")
	}
}
