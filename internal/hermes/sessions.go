package hermes

import (
	"context"
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

// Message is one stored turn of a Hermes session. Reasoning and tool payloads are
// deliberately not modelled: the voice assistant relays what was said, not how.
type Message struct {
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	Timestamp float64 `json:"timestamp"`
	ToolName  string  `json:"tool_name"`
}

// ChatReply is the synchronous answer from POST /api/sessions/{id}/chat.
type ChatReply struct {
	SessionID string
	Content   string
	Model     string
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

// SessionMessages returns stored user/assistant turns of a session, oldest first.
func (c *Client) SessionMessages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if !validID(sessionID) {
		return nil, errors.New("invalid Hermes session ID")
	}
	if limit < 1 || limit > 200 {
		return nil, errors.New("message limit must be between 1 and 200")
	}
	var result struct {
		Object    string    `json:"object"`
		SessionID string    `json:"session_id"`
		Data      []Message `json:"data"`
	}
	path := "/api/sessions/" + url.PathEscape(sessionID) + "/messages?limit=" + fmt.Sprint(limit)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &result); err != nil {
		return nil, err
	}
	if result.Object != "list" || result.SessionID != sessionID {
		return nil, errors.New("Hermes returned an invalid message list")
	}
	messages := result.Data[:0]
	for _, m := range result.Data {
		if strings.TrimSpace(m.Content) != "" && (m.Role == "user" || m.Role == "assistant") {
			messages = append(messages, m)
		}
	}
	return messages, nil
}

func (c *Client) CreateSession(ctx context.Context, title string) (Session, error) {
	if !c.Configured() {
		return Session{}, ErrNotConfigured
	}
	if len(title) > 256 {
		return Session{}, errors.New("session title must be at most 256 bytes")
	}
	body := map[string]any{}
	if strings.TrimSpace(title) != "" {
		body["title"] = title
	}
	var result sessionEnvelope
	if err := c.doJSON(ctx, http.MethodPost, "/api/sessions", body, nil, &result); err != nil {
		return Session{}, err
	}
	if result.Object != "hermes.session" || !validID(result.Session.ID) {
		return Session{}, errors.New("Hermes returned an invalid created session")
	}
	return result.Session, nil
}

// Chat runs one synchronous turn in an existing session. Hermes executes any
// tools itself before answering; this call blocks for the whole turn.
func (c *Client) Chat(ctx context.Context, sessionID, input string) (ChatReply, error) {
	if !c.Configured() {
		return ChatReply{}, ErrNotConfigured
	}
	if !validID(sessionID) || strings.TrimSpace(input) == "" || len(input) > MaxPromptBytes {
		return ChatReply{}, errors.New("chat requires a valid session ID and nonempty input of at most 64 KiB")
	}
	var result struct {
		Object    string `json:"object"`
		SessionID string `json:"session_id"`
		Message   struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		Runtime struct {
			Model string `json:"model"`
		} `json:"runtime"`
	}
	path := "/api/sessions/" + url.PathEscape(sessionID) + "/chat"
	if err := c.doJSONTimeout(ctx, http.MethodPost, path, map[string]string{"input": input}, nil, &result, chatTimeout); err != nil {
		return ChatReply{}, err
	}
	if result.Object != "hermes.session.chat.completion" || result.SessionID != sessionID || result.Message.Role != "assistant" {
		return ChatReply{}, errors.New("Hermes returned an invalid chat completion")
	}
	return ChatReply{SessionID: sessionID, Content: result.Message.Content, Model: result.Runtime.Model}, nil
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
