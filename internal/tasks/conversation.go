package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anguslees/talker/internal/hermes"
)

// Task.Conversation values.
const (
	RouteStarted   = "started"
	RouteContinued = "continued"
	// RouteSeparate is a request that would have continued the conversation but
	// ran in a new session because a Talker task was running there.
	RouteSeparate = "separate"
	// RouteSeparateExternal is the same for a turn the user started in another
	// Hermes client, which Talker cannot steer.
	RouteSeparateExternal = "separate_external"
)

// ErrSessionBusy refuses a second concurrent turn in one Hermes session:
// Hermes runs overlapping turns of a session without ordering them,
// interleaving their transcript writes.
var ErrSessionBusy = errors.New("a turn is still running in that Hermes session")

const (
	// conversationDayStart is the local time of day at which the conversation
	// rolls over, so late-night work stays in one conversation.
	conversationDayStart = 4 * time.Hour
	// externalTurnWindow is how long an unanswered message from another client
	// counts as a turn in progress; a failed turn stores no answer at all.
	externalTurnWindow = 10 * time.Minute
	// maxFormerSessions bounds the rotated-away session IDs a conversation keeps
	// for matching tasks still running under them.
	maxFormerSessions = 8
)

// conversation is the Hermes session that requests without an explicit session
// continue, so Hermes keeps its own record of earlier requests that day.
type conversation struct {
	// SessionID is empty until Hermes admits FoundingTask, whose run creates it.
	SessionID    string `json:"session_id,omitempty"`
	FoundingTask string `json:"founding_task,omitempty"`
	// Former lists session IDs the conversation had before compression rotated
	// it; Hermes keeps reporting a run under the session it was admitted to.
	Former       []string  `json:"former,omitempty"`
	ConnectionID string    `json:"connection_id"`
	Day          string    `json:"day"`
	StartedAt    time.Time `json:"started_at"`
}

func (c *conversation) sessions() []string {
	if c.SessionID == "" {
		return c.Former
	}
	return append([]string{c.SessionID}, c.Former...)
}

// Conversation describes the current conversation.
type Conversation struct {
	SessionID  string    `json:"session_id"`
	StartedAt  time.Time `json:"started_at"`
	ActiveTask string    `json:"active_task_id,omitempty"`
}

func conversationDay(t time.Time) string {
	return t.Local().Add(-conversationDayStart).Format(time.DateOnly)
}

// CurrentConversation reports the conversation that requests continue now.
func (m *Manager) CurrentConversation() (Conversation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.liveConversation(&m.state, m.now())
	if c == nil || c.SessionID == "" {
		return Conversation{}, false
	}
	return Conversation{SessionID: c.SessionID, StartedAt: c.StartedAt, ActiveTask: m.activeTask(&m.state, c.FoundingTask, c.sessions()...)}, true
}

// NewConversation ends the current conversation so the next request starts
// another. Running tasks are unaffected. It returns the ended session's ID.
func (m *Manager) NewConversation() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClosedLocked() {
		return "", ErrClosed
	}
	if m.state.Conversation == nil {
		return "", nil
	}
	previous := m.state.Conversation.SessionID
	next := cloneLedger(m.state)
	m.endConversation(&next)
	return previous, m.commitLocked(next)
}

// liveConversation returns the conversation requests continue at now, if any.
func (m *Manager) liveConversation(state *ledger, now time.Time) *conversation {
	c := state.Conversation
	if c == nil || c.ConnectionID != m.client.Identity() || c.Day != conversationDay(now) {
		return nil
	}
	if c.SessionID == "" {
		if rec, ok := state.Tasks[c.FoundingTask]; !ok || finished(rec.Task.Status) {
			return nil
		}
	}
	return c
}

// activeTask names an unfinished task of this connection writing to one of
// sessions, or the task founding, whose session may not be reported yet.
func (m *Manager) activeTask(state *ledger, founding string, sessions ...string) string {
	identity := m.client.Identity()
	for id, rec := range state.Tasks {
		if rec.ConnectionID != identity || finished(rec.Task.Status) {
			continue
		}
		if founding != "" && id == founding {
			return id
		}
		if session := rec.Task.HermesSession(); session != "" {
			for _, s := range sessions {
				if s == session {
					return id
				}
			}
		}
	}
	return ""
}

// runningSessions maps the live session of each unfinished Talker task to the
// task. Hermes keeps reporting a run under the session it was admitted to, so
// each is resolved through compression to the session now written to.
func (m *Manager) runningSessions(ctx context.Context) map[string]string {
	m.mu.Lock()
	identity := m.client.Identity()
	admitted := make(map[string]string)
	for id, rec := range m.state.Tasks {
		if session := rec.Task.HermesSession(); rec.ConnectionID == identity && !finished(rec.Task.Status) && session != "" {
			admitted[session] = id
		}
	}
	m.mu.Unlock()
	live := make(map[string]string, len(admitted))
	for session, id := range admitted {
		live[session] = id
		if page, err := m.client.SessionMessages(ctx, session, 1, 0); err == nil {
			live[page.SessionID] = id
		}
	}
	return live
}

// turnProbe is a session's newest page and its latest turn boundary.
type turnProbe struct {
	page hermes.MessagePage
	// openAt and digest describe the newest user message when no answer has
	// followed it; openAt is zero when the latest turn has finished.
	openAt float64
	digest string
}

// readSession reads a session's newest messages, resolving compression.
func (m *Manager) readSession(ctx context.Context, sessionID string, offset int) (hermes.MessagePage, error) {
	page, err := m.client.SessionMessages(ctx, sessionID, followPage, offset)
	if hermes.IsNotFound(err) {
		return page, fmt.Errorf("Hermes session not found: %w", err)
	}
	return page, err
}

// probeTurn pages back from the newest message to the latest user message or
// final answer, however many tool steps the running turn has taken.
func (m *Manager) probeTurn(ctx context.Context, sessionID string) (turnProbe, error) {
	var probe turnProbe
	for n := range followMaxPages {
		p, err := m.readSession(ctx, sessionID, n*followPage)
		if err != nil {
			return probe, err
		}
		if n == 0 {
			probe.page = p
		} else if p.SessionID != probe.page.SessionID {
			return probe, nil
		}
		for i := len(p.Messages) - 1; i >= 0; i-- {
			switch msg := p.Messages[i]; {
			case isAnswer(msg):
				return probe, nil
			case isAsk(msg):
				probe.openAt, probe.digest = msg.Timestamp, digest(msg.Content)
				return probe, nil
			}
		}
		if len(p.Messages) < followPage {
			return probe, nil
		}
	}
	return probe, nil
}

// conversationToProbe names the conversation session whose newest turn a
// request continuing it must check, or "" when a Talker task already makes the
// conversation busy or there is none.
func (m *Manager) conversationToProbe() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.liveConversation(&m.state, m.now())
	if c == nil || c.SessionID == "" || m.activeTask(&m.state, c.FoundingTask, c.sessions()...) != "" {
		return ""
	}
	return c.SessionID
}

func externalTurnRunning(openedAt float64, now time.Time) bool {
	return openedAt != 0 && now.Sub(time.Unix(int64(openedAt), 0)) < externalTurnWindow
}

// externalTurnOpen reports whether another client's turn may still be running
// in a session: its newest user message is recent, unanswered and not one
// Talker submitted. A fresh probe decides; otherwise the session's follow does.
func (m *Manager) externalTurnOpen(state *ledger, probe *turnProbe, now time.Time, sessions ...string) bool {
	if probe != nil {
		return externalTurnRunning(probe.openAt, now) && !m.submittedByTalker(state, probe.digest, "")
	}
	id := m.findFollow(state, sessions...)
	return id != "" && externalTurnRunning(state.Follows[id].OpenTurnAt, now)
}

// route picks the session for a request that named none, founding a new
// conversation when there is no live one. probe, when set, is a fresh read of
// session probed; running is from runningSessions.
func (m *Manager) route(next *ledger, taskID, probed string, probe *turnProbe, running map[string]string, now time.Time) (sessionID, route string) {
	c := m.liveConversation(next, now)
	if c == nil {
		m.endConversation(next)
		next.Conversation = &conversation{FoundingTask: taskID, ConnectionID: m.client.Identity(), Day: conversationDay(now), StartedAt: now}
		return "", RouteStarted
	}
	if c.SessionID == "" || running[c.SessionID] != "" || m.activeTask(next, c.FoundingTask, c.sessions()...) != "" {
		return "", RouteSeparate
	}
	if probed != c.SessionID {
		probe = nil
	}
	if m.externalTurnOpen(next, probe, now, c.sessions()...) {
		return "", RouteSeparateExternal
	}
	return c.SessionID, RouteContinued
}

// adopt makes the session a request named the conversation, refusing it while
// a turn runs there. probe is a fresh read of requested; running is from
// runningSessions.
func (m *Manager) adopt(next *ledger, requested string, probe turnProbe, running map[string]string, now time.Time) error {
	tip := probe.page.SessionID
	sessions := []string{tip, requested}
	founding := ""
	c := m.liveConversation(next, now)
	current := c != nil && (containsString(c.sessions(), tip) || containsString(c.sessions(), requested))
	if current {
		founding, sessions = c.FoundingTask, append(sessions, c.sessions()...)
	}
	if id := m.findFollow(next, sessions...); id != "" && next.Follows[id].Follow.Origin != "" {
		sessions = append(sessions, next.Follows[id].Follow.Origin)
	}
	busy := running[tip]
	if busy == "" {
		busy = m.activeTask(next, founding, sessions...)
	}
	if busy != "" {
		return fmt.Errorf("%w: Talker task %s; steer it or wait for it to finish", ErrSessionBusy, busy)
	}
	if m.externalTurnOpen(next, &probe, now) {
		return fmt.Errorf("%w: its newest message, from another Hermes client, has no answer yet", ErrSessionBusy)
	}
	if current {
		m.repoint(next, c.SessionID, tip)
		return nil
	}
	m.endConversation(next)
	next.Conversation = &conversation{SessionID: tip, ConnectionID: m.client.Identity(), Day: conversationDay(now), StartedAt: now}
	if requested != tip {
		next.Conversation.Former = []string{requested}
	}
	id, err := m.attachFollow(next, requested, probe.page, "", now)
	if err != nil {
		return err
	}
	rec := next.Follows[id]
	rec.Follow.Conversation = true
	next.Follows[id] = rec
	return nil
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// endConversation clears the conversation and the follow that announced it,
// keeping that follow only if the user asked for it explicitly.
func (m *Manager) endConversation(next *ledger) {
	next.Conversation = nil
	for id, rec := range next.Follows {
		if !rec.Follow.Conversation {
			continue
		}
		if rec.Follow.Explicit {
			rec.Follow.Conversation = false
			next.Follows[id] = rec
		} else {
			delete(next.Follows, id)
		}
	}
}

// trackConversation keeps the conversation and follows pointed at the session
// Hermes actually uses, and reports whether it changed next.
func (m *Manager) trackConversation(next *ledger, before, after Task) bool {
	changed := false
	if c := next.Conversation; c != nil && c.FoundingTask == after.ID && c.SessionID == "" {
		switch {
		case after.RunID != "":
			c.SessionID = after.HermesSession()
			m.followConversation(next, c)
			changed = true
		case finished(after.Status):
			next.Conversation = nil
			changed = true
		}
	}
	if from, to := before.HermesSession(), after.HermesSession(); from != "" && to != from {
		changed = m.repoint(next, from, to) || changed
	}
	return changed
}

// Finished reports a task Hermes will not advance further.
func (t Task) Finished() bool { return finished(t.Status) }

// HermesSession is the session the task's run writes to: Hermes uses the run
// ID as the session of a run that selects none.
func (t Task) HermesSession() string {
	if t.SessionID != "" {
		return t.SessionID
	}
	return t.RunID
}

// repoint moves the conversation and follows from a session to the
// continuation Hermes reported for it after compression.
func (m *Manager) repoint(next *ledger, from, to string) bool {
	if from == "" || to == "" || from == to {
		return false
	}
	changed := false
	if c := next.Conversation; c != nil && c.SessionID == from {
		c.SessionID = to
		if !containsString(c.Former, from) {
			c.Former = append([]string{from}, c.Former...)[:min(len(c.Former)+1, maxFormerSessions)]
		}
		changed = true
	}
	for id, rec := range next.Follows {
		if rec.Follow.SessionID == from {
			rec.Follow.SessionID = to
			if rec.Follow.Origin == "" {
				rec.Follow.Origin = from
			}
			rec.Count = -1
			next.Follows[id] = rec
			changed = true
		}
	}
	return changed
}

// isAsk reports a user message that opens a turn; compaction handoffs are
// served as hidden rows with no content.
func isAsk(msg hermes.Message) bool {
	return msg.Role == "user" && msg.DisplayKind != "hidden" && strings.TrimSpace(msg.Content) != ""
}

// isAnswer reports the assistant message that ends a turn.
func isAnswer(msg hermes.Message) bool {
	return msg.Role == "assistant" && msg.FinishReason == "stop" && msg.DisplayKind != "hidden" && strings.TrimSpace(msg.Content) != ""
}
