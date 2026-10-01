package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/anguslees/talker/internal/hermes"
)

const (
	followPage     = 20
	followMaxPages = 10
	// followIdle ends an explicit follow whose session has been quiet this long.
	followIdle = 24 * time.Hour
	// submittedWindow bounds the tasks compared against a followed turn.
	submittedWindow = 48 * time.Hour
	// followAnswers is how many recent answers a follow remembers; compaction
	// re-inserts the last few turns verbatim under new message IDs.
	followAnswers = 16
)

// Follow is a Hermes session whose new replies Talker announces, whichever
// client added the turn: a web UI, the terminal, or Talker itself.
type Follow struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	// Origin is the session ID originally followed when compression has since
	// moved the conversation to SessionID.
	Origin       string    `json:"origin,omitempty"`
	Title        string    `json:"title,omitempty"`
	Explicit     bool      `json:"explicit,omitempty"`
	Conversation bool      `json:"conversation,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	ActiveAt     time.Time `json:"active_at"`
}

type followRecord struct {
	Follow       Follow `json:"follow"`
	ConnectionID string `json:"connection_id"`
	// Count and EndedAt are the session row as last read; a change prompts a
	// read of new messages. -1 forces the next read.
	Count   int     `json:"count"`
	EndedAt float64 `json:"ended_at,omitempty"`
	// LastMessageID is the newest message already processed, and SeenAt the
	// newest Hermes timestamp among them.
	LastMessageID int64   `json:"last_message_id"`
	SeenAt        float64 `json:"seen_at,omitempty"`
	// TurnDigest identifies the user message opening an unfinished turn, so a
	// turn Talker submitted is left to its task's own notification.
	TurnDigest string `json:"turn_digest,omitempty"`
	// OpenTurnAt is the Hermes timestamp of an unanswered user message from
	// another client, whose turn may still be running.
	OpenTurnAt float64 `json:"open_turn_at,omitempty"`
	// Answers are digests of the newest answers processed, so a copy that
	// compaction re-inserts is not announced again.
	Answers []string `json:"answers,omitempty"`
}

// followDelayFor is the wait before a follow's next poll.
var followDelayFor = followDelay

func digest(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:])
}

// Follow announces replies added to an existing Hermes session from now on.
func (m *Manager) Follow(ctx context.Context, sessionID string) (Follow, error) {
	if !m.client.Configured() {
		return Follow{}, hermes.ErrNotConfigured
	}
	page, err := m.readSession(ctx, sessionID, 0)
	if err != nil {
		return Follow{}, err
	}
	session, err := m.client.GetSession(ctx, page.SessionID)
	if err != nil {
		return Follow{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClosedLocked() {
		return Follow{}, ErrClosed
	}
	now := m.now().UTC()
	next := cloneLedger(m.state)
	id, err := m.attachFollow(&next, sessionID, page, firstLine(session.Title, session.Preview), now)
	if err != nil {
		return Follow{}, err
	}
	rec := next.Follows[id]
	rec.Follow.Explicit, rec.Follow.ActiveAt = true, now
	next.Follows[id] = rec
	return rec.Follow, m.commitLocked(next)
}

// attachFollow returns the follow of the session requested, which page read at
// its live continuation, creating one that starts after the newest message:
// replies already stored were answered before the follow began.
func (m *Manager) attachFollow(next *ledger, requested string, page hermes.MessagePage, title string, now time.Time) (string, error) {
	if id := m.findFollow(next, page.SessionID, requested); id != "" {
		m.repoint(next, next.Follows[id].Follow.SessionID, page.SessionID)
		return id, nil
	}
	id, err := newID("follow_")
	if err != nil {
		return "", err
	}
	rec := followRecord{
		Follow:       Follow{ID: id, SessionID: page.SessionID, Title: title, CreatedAt: now, ActiveAt: now},
		ConnectionID: m.client.Identity(),
		Count:        -1,
	}
	if requested != page.SessionID {
		rec.Follow.Origin = requested
	}
	if _, err := m.advanceFollow(next, &rec, page.Messages, false); err != nil {
		return "", err
	}
	next.Follows[id] = rec
	return id, nil
}

// Unfollow stops announcing a session, reporting whether it was followed.
func (m *Manager) Unfollow(sessionID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClosedLocked() {
		return false, ErrClosed
	}
	next := cloneLedger(m.state)
	id := m.findFollow(&next, sessionID, sessionID)
	if id == "" {
		return false, nil
	}
	delete(next.Follows, id)
	return true, m.commitLocked(next)
}

// Follows lists followed sessions, oldest first.
func (m *Manager) Follows() []Follow {
	m.mu.Lock()
	defer m.mu.Unlock()
	identity := m.client.Identity()
	result := make([]Follow, 0, len(m.state.Follows))
	for _, rec := range m.state.Follows {
		if rec.ConnectionID == identity {
			result = append(result, rec.Follow)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func (m *Manager) findFollow(state *ledger, ids ...string) string {
	identity := m.client.Identity()
	for id, rec := range state.Follows {
		if rec.ConnectionID != identity {
			continue
		}
		for _, sessionID := range ids {
			if rec.Follow.SessionID == sessionID || rec.Follow.Origin == sessionID {
				return id
			}
		}
	}
	return ""
}

// followConversation makes the follow of c's session announce it as the
// current conversation.
func (m *Manager) followConversation(next *ledger, c *conversation) {
	if id := m.findFollow(next, c.SessionID); id != "" {
		rec := next.Follows[id]
		rec.Follow.Conversation = true
		next.Follows[id] = rec
		return
	}
	id, err := newID("follow_")
	if err != nil {
		return
	}
	now := m.now().UTC()
	// A zero cursor reads the whole new session; the turns Talker submitted are
	// recognised by their prompts and left to their tasks.
	next.Follows[id] = followRecord{
		Follow:       Follow{ID: id, SessionID: c.SessionID, Conversation: true, CreatedAt: now, ActiveAt: now},
		ConnectionID: c.ConnectionID,
		Count:        -1,
	}
}

func (m *Manager) startFollowersLocked() {
	if m.isClosedLocked() || !m.client.Configured() {
		return
	}
	identity := m.client.Identity()
	for id, rec := range m.state.Follows {
		if rec.ConnectionID != identity || m.followers[id] {
			continue
		}
		m.followers[id] = true
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer func() {
				m.mu.Lock()
				delete(m.followers, id)
				m.mu.Unlock()
			}()
			m.followLoop(id)
		}()
	}
}

func (m *Manager) followLoop(id string) {
	failures, missing := 0, 0
	for delay := time.Duration(0); wait(m.ctx, delay); {
		rec, ok := m.followReady(id)
		if !ok {
			return
		}
		err := m.pollFollow(id, rec)
		switch {
		case err == nil:
			failures, missing = 0, 0
		case hermes.IsNotFound(err):
			if missing++; missing >= 3 {
				m.dropFollow(id)
				return
			}
			failures++
		default:
			failures++
		}
		quiet, ok := m.followQuiet(id)
		if !ok {
			return
		}
		delay = followDelayFor(quiet, failures)
	}
}

// followDelay polls quickly while a session is in use and slowly once quiet.
func followDelay(quiet time.Duration, failures int) time.Duration {
	delay := 60 * time.Second
	switch {
	case quiet < 2*time.Minute:
		delay = 5 * time.Second
	case quiet < 30*time.Minute:
		delay = 15 * time.Second
	}
	if failures > 0 {
		delay = max(delay, min(time.Duration(1<<min(failures, 6))*time.Second, 60*time.Second))
	}
	return delay
}

// followQuiet reports how long a followed session has been quiet.
func (m *Manager) followQuiet(id string) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.state.Follows[id]
	return m.now().Sub(rec.Follow.ActiveAt), ok && !m.isClosedLocked()
}

// followReady ends a follow whose purpose has lapsed and reports whether the
// follow should be polled.
func (m *Manager) followReady(id string) (followRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.state.Follows[id]
	if !ok || m.isClosedLocked() || rec.ConnectionID != m.client.Identity() {
		return rec, false
	}
	now := m.now()
	live := m.liveConversation(&m.state, now)
	stale := rec.Follow.Conversation && (live == nil || live.SessionID != rec.Follow.SessionID)
	idle := rec.Follow.Explicit && !rec.Follow.Conversation && now.Sub(rec.Follow.ActiveAt) > followIdle
	if !stale && !idle {
		return rec, true
	}
	next := cloneLedger(m.state)
	if stale && live == nil {
		m.endConversation(&next)
	}
	if cur, ok := next.Follows[id]; ok && (idle || (cur.Follow.Conversation && !cur.Follow.Explicit)) {
		delete(next.Follows, id)
	} else if ok && cur.Follow.Conversation {
		cur.Follow.Conversation = false
		next.Follows[id] = cur
	}
	if err := m.commitLocked(next); err != nil {
		return rec, true
	}
	rec, ok = m.state.Follows[id]
	return rec, ok
}

func (m *Manager) dropFollow(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.state.Follows[id]
	if !ok || m.isClosedLocked() {
		return
	}
	next := cloneLedger(m.state)
	delete(next.Follows, id)
	if next.Conversation != nil && next.Conversation.SessionID == rec.Follow.SessionID {
		m.endConversation(&next)
	}
	_ = m.commitLocked(next)
}

// pollFollow announces replies added since the last poll. The session row is
// cheap to read; messages are read only when its count or end changes. A
// session is not read while a Talker task writes to it: that turn announces
// itself, and Hermes creates a new run's session only once its turn starts.
func (m *Manager) pollFollow(id string, rec followRecord) error {
	m.mu.Lock()
	busy := m.activeTask(&m.state, "", rec.Follow.SessionID, rec.Follow.Origin) != ""
	m.mu.Unlock()
	if busy {
		return nil
	}
	session, err := m.client.GetSession(m.ctx, rec.Follow.SessionID)
	if err != nil {
		return err
	}
	if session.MessageCount == rec.Count && session.EndedAt == rec.EndedAt {
		return nil
	}
	rows, tip, err := m.newMessages(rec)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClosedLocked() {
		return ErrClosed
	}
	cur, ok := m.state.Follows[id]
	if !ok || cur.LastMessageID != rec.LastMessageID || cur.Follow.SessionID != rec.Follow.SessionID {
		return nil
	}
	next := cloneLedger(m.state)
	moved := tip != cur.Follow.SessionID
	if moved {
		// The row read belongs to the closed session; the continuation's is read next.
		m.repoint(&next, cur.Follow.SessionID, tip)
		cur = next.Follows[id]
	} else {
		cur.Count, cur.EndedAt = session.MessageCount, session.EndedAt
		if title := firstLine(session.Title, session.Preview); title != "" {
			cur.Follow.Title = title
		}
	}
	events, err := m.advanceFollow(&next, &cur, rows, true)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		cur.Follow.ActiveAt = m.now().UTC()
	}
	next.Events = append(next.Events, events...)
	next.Follows[id] = cur
	ended := !moved && session.EndedAt != 0 && hermes.IsConversationBoundary(session.EndReason)
	if ended {
		delete(next.Follows, id)
		if next.Conversation != nil && next.Conversation.SessionID == cur.Follow.SessionID {
			m.endConversation(&next)
		}
	}
	if len(events) == 0 && !moved && !ended {
		// Only the read position moved. It is saved with the next committed
		// change; re-reading these rows after a crash announces nothing.
		m.state = next
		return nil
	}
	return m.commitLocked(next)
}

// newMessages reads messages newer than the follow's cursor, oldest first, and
// the session that holds them, which differs after compression.
func (m *Manager) newMessages(rec followRecord) ([]hermes.Message, string, error) {
	seen := make(map[int64]bool)
	var rows []hermes.Message
	tip := ""
	for page := range followMaxPages {
		p, err := m.client.SessionMessages(m.ctx, rec.Follow.SessionID, followPage, page*followPage)
		if err != nil {
			return nil, "", err
		}
		if tip == "" {
			tip = p.SessionID
		} else if p.SessionID != tip {
			break
		}
		reached := len(p.Messages) < followPage
		for _, msg := range p.Messages {
			if msg.ID <= rec.LastMessageID {
				reached = true
			} else if !seen[msg.ID] {
				seen[msg.ID] = true
				rows = append(rows, msg)
			}
		}
		if reached {
			break
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, tip, nil
}

// advanceFollow moves the cursor over rows and, when announce is set, returns
// a notification for each finished turn not submitted by Talker.
func (m *Manager) advanceFollow(state *ledger, rec *followRecord, rows []hermes.Message, announce bool) ([]Event, error) {
	var events []Event
	for _, msg := range rows {
		rec.LastMessageID = max(rec.LastMessageID, msg.ID)
		if msg.DisplayKind == "hidden" {
			// Compaction scaffolding, stamped when compaction ran.
			continue
		}
		// Compaction re-inserts recent turns under new IDs with their original
		// timestamps; a copy of a row already processed only moves the cursor.
		copied := msg.Timestamp < rec.SeenAt
		rec.SeenAt = max(rec.SeenAt, msg.Timestamp)
		switch {
		case copied:
		case isAsk(msg):
			rec.TurnDigest, rec.OpenTurnAt = digest(msg.Content), 0
			if !m.submittedByTalker(state, rec.TurnDigest, "") {
				rec.OpenTurnAt = msg.Timestamp
			}
		case isAnswer(msg):
			answer := digest(msg.Content)
			if announce && !containsString(rec.Answers, answer) && !m.submittedByTalker(state, rec.TurnDigest, answer) {
				id, err := newID("event_")
				if err != nil {
					return nil, err
				}
				title := rec.Follow.Title
				if title == "" {
					title = rec.Follow.SessionID
				}
				events = append(events, Event{
					ID: id, SessionID: rec.Follow.SessionID, Title: "Hermes replied in " + truncateRunes(title, 200),
					Body: truncateRunes(msg.Content, 16000), CreatedAt: m.now().UTC(),
				})
			}
			rec.TurnDigest, rec.OpenTurnAt = "", 0
			if !containsString(rec.Answers, answer) {
				rec.Answers = append(rec.Answers, answer)[max(len(rec.Answers)+1-followAnswers, 0):]
			}
		}
	}
	return events, nil
}

// submittedByTalker recognises a turn from one of Talker's own recent runs,
// which Hermes stores with the prompt verbatim as its user message.
func (m *Manager) submittedByTalker(state *ledger, turn, answer string) bool {
	identity := m.client.Identity()
	since := m.now().Add(-submittedWindow)
	for _, rec := range state.Tasks {
		if rec.ConnectionID != identity || rec.Task.UpdatedAt.Before(since) {
			continue
		}
		if (turn != "" && digest(rec.Task.Prompt) == turn) || (answer != "" && rec.Task.Output != "" && digest(rec.Task.Output) == answer) {
			return true
		}
	}
	return false
}

func firstLine(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(strings.SplitN(v, "\n", 2)[0]); v != "" {
			return truncateRunes(v, 200)
		}
	}
	return ""
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
