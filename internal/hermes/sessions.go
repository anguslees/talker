package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Session is a Hermes conversation as listed by GET /api/sessions. Runs
// submitted through the API appear here with their run ID as the session ID.
type Session struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Preview       string  `json:"preview"`
	Source        string  `json:"source"`
	Model         string  `json:"model"`
	StartedAt     float64 `json:"started_at"`
	LastActive    float64 `json:"last_active"`
	EndedAt       float64 `json:"ended_at"`
	EndReason     string  `json:"end_reason"`
	MessageCount  int     `json:"message_count"`
	ToolCallCount int     `json:"tool_call_count"`
	Archived      bool    `json:"archived"`
}

// Message is one stored row of a Hermes session. Reasoning and tool payloads are
// deliberately not modelled: the voice assistant relays what was said, not how.
type Message struct {
	// ID is Hermes's global insertion order across all sessions, so it keeps
	// increasing when compression moves a conversation to a new session.
	ID        int64
	Role      string
	Content   string
	Timestamp float64
	ToolName  string
	// FinishReason "stop" marks the assistant row that ends a turn; intermediate
	// tool-calling steps report "tool_calls".
	FinishReason string
	// DisplayKind "hidden" marks rows clients do not show, such as a compaction
	// handoff, whose content Hermes serves empty.
	DisplayKind string
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID           int64           `json:"id"`
		Role         string          `json:"role"`
		Content      json.RawMessage `json:"content"`
		Timestamp    float64         `json:"timestamp"`
		ToolName     string          `json:"tool_name"`
		FinishReason string          `json:"finish_reason"`
		DisplayKind  string          `json:"display_kind"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = Message{ID: raw.ID, Role: raw.Role, Content: contentText(raw.Content), Timestamp: raw.Timestamp, ToolName: raw.ToolName, FinishReason: raw.FinishReason, DisplayKind: raw.DisplayKind}
	return nil
}

// contentText accepts both a plain string and the multimodal list of parts that
// clients such as web UIs store when a turn carries attachments.
func contentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part.Text) != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// MessagePage is one page of a session's stored rows, oldest first.
type MessagePage struct {
	// SessionID is the transcript Hermes actually read: a compressed session
	// resolves to its live continuation.
	SessionID string
	Messages  []Message
}

// Health is GET /health: unauthenticated liveness plus version.
type Health struct {
	Status   string `json:"status"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
}

// ModelOptions is the current route from GET /api/model/options; the provider
// catalogue behind it is large and not something to read aloud.
type ModelOptions struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

type sessionEnvelope struct {
	Object  string  `json:"object"`
	Session Session `json:"session"`
}

// IsNotFound reports a Hermes 404, such as an unknown session.
func IsNotFound(err error) bool {
	var remote *HTTPError
	return errors.As(err, &remote) && remote.StatusCode == http.StatusNotFound
}

// IsConversationBoundary reports an end_reason that deliberately closes a
// conversation, mirroring _BOUNDARY_END_REASONS in hermes_state_common.py plus
// the "branched" end of a fork. Automatic ends (agent_close, cli_close) and
// "compression", whose conversation continues in a child session, are not.
func IsConversationBoundary(reason string) bool {
	switch reason {
	case "session_reset", "session_switch", "idle", "daily", "suspended", "resume_pending_expired", "new_session", "branched":
		return true
	}
	return false
}

func (c *Client) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("session limit must be between 1 and 100")
	}
	var result struct {
		Object string    `json:"object"`
		Data   []Session `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/sessions?limit="+fmt.Sprint(limit), nil, nil, &result); err != nil {
		return nil, err
	}
	if result.Object != "list" {
		return nil, errors.New("Hermes returned an invalid session list")
	}
	return result.Data, nil
}

// GetSession reads the exact session row; unlike SessionMessages it does not
// follow compression to a continuation.
func (c *Client) GetSession(ctx context.Context, sessionID string) (Session, error) {
	if !c.Configured() {
		return Session{}, ErrNotConfigured
	}
	if !validID(sessionID) {
		return Session{}, errors.New("invalid Hermes session ID")
	}
	var result sessionEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/sessions/"+url.PathEscape(sessionID), nil, nil, &result); err != nil {
		return Session{}, err
	}
	if result.Object != "hermes.session" || result.Session.ID != sessionID {
		return Session{}, errors.New("Hermes returned an invalid session")
	}
	return result.Session, nil
}

// SessionMessages pages back from a session's newest rows: offset skips that
// many of the newest. Hermes returns the oldest rows instead whenever a limit
// is sent without order=latest.
func (c *Client) SessionMessages(ctx context.Context, sessionID string, limit, offset int) (MessagePage, error) {
	if !c.Configured() {
		return MessagePage{}, ErrNotConfigured
	}
	if !validID(sessionID) {
		return MessagePage{}, errors.New("invalid Hermes session ID")
	}
	if limit < 1 || limit > 500 || offset < 0 || offset > 100_000 {
		return MessagePage{}, errors.New("message page must have a limit of 1-500 and a non-negative offset")
	}
	var result struct {
		Object    string    `json:"object"`
		SessionID string    `json:"session_id"`
		Data      []Message `json:"data"`
	}
	path := fmt.Sprintf("/api/sessions/%s/messages?order=latest&limit=%d&offset=%d", url.PathEscape(sessionID), limit, offset)
	if err := c.doJSONLimit(ctx, http.MethodGet, path, nil, nil, &result, maxMessagePageBytes); err != nil {
		return MessagePage{}, err
	}
	if result.Object != "list" || !validID(result.SessionID) || len(result.Data) > limit {
		return MessagePage{}, errors.New("Hermes returned an invalid message list")
	}
	return MessagePage{SessionID: result.SessionID, Messages: result.Data}, nil
}

// Conversational keeps the user and assistant rows that carry text, dropping
// tool results and empty tool-calling steps.
func Conversational(messages []Message) []Message {
	kept := make([]Message, 0, len(messages))
	for _, m := range messages {
		if strings.TrimSpace(m.Content) != "" && (m.Role == "user" || m.Role == "assistant") {
			kept = append(kept, m)
		}
	}
	return kept
}

func (c *Client) ModelOptions(ctx context.Context) (ModelOptions, error) {
	if !c.Configured() {
		return ModelOptions{}, ErrNotConfigured
	}
	var result ModelOptions
	if err := c.doJSON(ctx, http.MethodGet, "/api/model/options", nil, nil, &result); err != nil {
		return ModelOptions{}, err
	}
	if result.Model == "" {
		return ModelOptions{}, errors.New("Hermes returned no current model")
	}
	return result, nil
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	if !c.Configured() {
		return Health{}, ErrNotConfigured
	}
	var result Health
	if err := c.doJSON(ctx, http.MethodGet, "/health", nil, nil, &result); err != nil {
		return Health{}, err
	}
	if result.Status == "" {
		return Health{}, errors.New("Hermes returned an invalid health response")
	}
	return result, nil
}
