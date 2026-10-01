// Package hermes implements the Hermes API Server HTTP contract, not the dashboard RPC API.
package hermes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	requestTimeout   = 20 * time.Second
	maxResponseBytes = 2 << 20
	// Message pages carry raw tool output and reasoning, each row up to ~200 KB.
	maxMessagePageBytes = 16 << 20
	maxRequestBytes     = 512 << 10
	MaxPromptBytes      = 64 << 10
)

var ErrNotConfigured = errors.New("Hermes is not configured: set the HTTP API base URL and bearer key")

type Client struct {
	base       *url.URL
	key        string
	http       *http.Client
	streamHTTP *http.Client
}

// New accepts the listener root, optionally with a /p/<profile> prefix. An empty
// URL creates a disabled client whose operations return ErrNotConfigured.
func New(baseURL, key string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return &Client{}, nil
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("Hermes base URL must be an http(s) listener URL without credentials, query, or fragment")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("HERMES_URL is set but HERMES_API_KEY is empty: set it to the Hermes gateway's API_SERVER_KEY, or unset HERMES_URL to run without Hermes")
	}
	for _, ch := range key {
		if ch < 33 || ch > 126 {
			return nil, errors.New("Hermes bearer key must contain only visible ASCII characters")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 5 * time.Second
	transport.ResponseHeaderTimeout = 10 * time.Second
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxConnsPerHost = 32
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// Long-lived SSE streams must not consume the JSON pool's stop and polling slots.
	slow := transport.Clone()
	slow.ResponseHeaderTimeout = streamIdleLimit
	return &Client{
		base: u, key: key,
		http:       &http.Client{Transport: transport, CheckRedirect: noRedirect},
		streamHTTP: &http.Client{Transport: slow, CheckRedirect: noRedirect},
	}, nil
}

func (c *Client) Configured() bool { return c != nil && c.base != nil && c.key != "" }

// Identity binds recovery to the same URL/profile and credential without storing a secret.
func (c *Client) Identity() string {
	if !c.Configured() {
		return ""
	}
	sum := sha256.Sum256([]byte(c.base.String() + "\x00" + c.key))
	return hex.EncodeToString(sum[:])
}

type Endpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type IdempotencyCapabilities struct {
	Supported        bool  `json:"supported"`
	Durable          bool  `json:"durable"`
	RetentionSeconds int64 `json:"retention_seconds"`
}

type Capabilities struct {
	Object   string `json:"object"`
	Platform string `json:"platform"`
	Model    string `json:"model"`
	Auth     struct {
		Type     string `json:"type"`
		Required bool   `json:"required"`
	} `json:"auth"`
	Features struct {
		RunSubmission       bool                    `json:"run_submission"`
		RunStatus           bool                    `json:"run_status"`
		RunEventsSSE        bool                    `json:"run_events_sse"`
		RunStop             bool                    `json:"run_stop"`
		RunSteer            bool                    `json:"run_steer"`
		RunApprovalResponse bool                    `json:"run_approval_response"`
		RunsIdempotency     IdempotencyCapabilities `json:"runs_idempotency"`
		AudioAPI            bool                    `json:"audio_api"`
		RealtimeVoice       bool                    `json:"realtime_voice"`
	} `json:"features"`
	Endpoints map[string]Endpoint `json:"endpoints"`
}

type RunRequest struct {
	Input     string `json:"input"`
	SessionID string `json:"session_id,omitempty"`
}

type Admission struct {
	RunID    string `json:"run_id"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
}

type Approval struct {
	RequestID   string   `json:"request_id"`
	Command     string   `json:"command,omitempty"`
	Description string   `json:"description,omitempty"`
	Choices     []string `json:"choices,omitempty"`
}

type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type Runtime struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	RouteSource string `json:"route_source"`
}

type Run struct {
	Object              string          `json:"object"`
	RunID               string          `json:"run_id"`
	SessionID           string          `json:"session_id"`
	Status              string          `json:"status"`
	Model               string          `json:"model,omitempty"`
	Output              string          `json:"output,omitempty"`
	Error               string          `json:"error,omitempty"`
	CreatedAt           float64         `json:"created_at,omitempty"`
	UpdatedAt           float64         `json:"updated_at,omitempty"`
	ShutdownRequestedAt float64         `json:"shutdown_requested_at,omitempty"`
	Approval            *Approval       `json:"approval,omitempty"`
	Usage               *Usage          `json:"usage,omitempty"`
	Runtime             *Runtime        `json:"runtime,omitempty"`
	PendingSteer        json.RawMessage `json:"pending_steer,omitempty"`
}

func IsTerminal(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "interrupted":
		return true
	}
	return false
}

func validStatus(status string) bool {
	return IsTerminal(status) || status == "queued" || status == "running" || status == "waiting_for_approval" || status == "stopping"
}

type HTTPError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("Hermes HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("Hermes HTTP %d: %s", e.StatusCode, e.Message)
}

// Retryable includes ambiguous transport/decoding failures: an admission may have
// succeeded before its response was lost, so callers must retain the same key.
func Retryable(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= 500 || httpErr.StatusCode == 408 || httpErr.StatusCode == 429
	}
	return !errors.Is(err, ErrNotConfigured)
}

func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var result Capabilities
	err := c.doJSON(ctx, http.MethodGet, "/v1/capabilities", nil, nil, &result)
	if err == nil && result.Object != "hermes.api_server.capabilities" {
		err = errors.New("Hermes returned an invalid capabilities response")
	}
	return result, err
}

func (c *Client) CreateRun(ctx context.Context, input RunRequest, idempotencyKey string) (Admission, error) {
	if strings.TrimSpace(input.Input) == "" || len(input.Input) > MaxPromptBytes {
		return Admission{}, errors.New("Hermes input must be nonempty and at most 64 KiB")
	}
	if idempotencyKey == "" || len(idempotencyKey) > 255 || strings.IndexFunc(idempotencyKey, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return Admission{}, errors.New("Hermes Idempotency-Key must be 1-255 visible ASCII characters")
	}
	var result Admission
	err := c.doJSON(ctx, http.MethodPost, "/v1/runs", input, http.Header{"Idempotency-Key": {idempotencyKey}}, &result)
	if err == nil && (!validID(result.RunID) || (result.Status != "started" && !validStatus(result.Status))) {
		err = errors.New("Hermes returned an invalid run admission")
	}
	return result, err
}

func (c *Client) GetRun(ctx context.Context, runID string) (Run, error) {
	if !validID(runID) {
		return Run{}, errors.New("invalid Hermes run ID")
	}
	var run Run
	err := c.doJSON(ctx, http.MethodGet, "/v1/runs/"+runID, nil, nil, &run)
	if err == nil && (run.RunID != runID || !validStatus(run.Status)) {
		err = errors.New("Hermes returned an invalid run status")
	}
	return run, err
}

func (c *Client) StopRun(ctx context.Context, runID string) (Run, error) {
	if !validID(runID) {
		return Run{}, errors.New("invalid Hermes run ID")
	}
	var run Run
	err := c.doJSON(ctx, http.MethodPost, "/v1/runs/"+runID+"/stop", nil, nil, &run)
	if err == nil && (run.RunID != runID || !validStatus(run.Status)) {
		err = errors.New("Hermes returned an invalid stop response")
	}
	return run, err
}

func (c *Client) SteerRun(ctx context.Context, runID, input string) error {
	if !validID(runID) || strings.TrimSpace(input) == "" || len(input) > MaxPromptBytes {
		return errors.New("steer requires a valid run ID and nonempty input of at most 64 KiB")
	}
	var result struct {
		RunID    string `json:"run_id"`
		Accepted bool   `json:"accepted"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/runs/"+runID+"/steer", map[string]string{"input": input}, nil, &result); err != nil {
		return err
	}
	if result.RunID != runID || !result.Accepted {
		return errors.New("Hermes did not accept the steer request")
	}
	return nil
}

func (c *Client) ApproveRun(ctx context.Context, runID, requestID, choice string) error {
	if !validID(runID) || strings.TrimSpace(requestID) == "" || len(requestID) > 256 {
		return errors.New("approval requires a valid run ID and exact approval request ID")
	}
	switch choice {
	case "once", "session", "always", "deny":
	default:
		return errors.New("invalid Hermes approval choice")
	}
	var result struct {
		RunID    string `json:"run_id"`
		Resolved int    `json:"resolved"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/runs/"+runID+"/approval", map[string]string{"request_id": requestID, "choice": choice}, nil, &result); err != nil {
		return err
	}
	if result.RunID != runID || result.Resolved < 1 {
		return errors.New("Hermes did not resolve the approval")
	}
	return nil
}

func validID(id string) bool {
	return id != "" && len(id) <= 256 && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) == -1
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	u := *c.base
	// A "?query" suffix on path is a real query string, not path characters.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		u.RawQuery = path[i+1:]
		path = path[:i]
	}
	u.Path += path
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, headers http.Header, result any) error {
	return c.doJSONLimit(ctx, method, path, body, headers, result, maxResponseBytes)
}

func (c *Client) doJSONLimit(ctx context.Context, method, path string, body any, headers http.Header, result any, limit int) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if len(data) > maxRequestBytes {
			return errors.New("Hermes request exceeds size limit")
		}
		reader = bytes.NewReader(data)
	}
	req, err := c.request(ctx, method, path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		req.Header[key] = values
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Hermes request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return fmt.Errorf("read Hermes response: %w", err)
	}
	if len(data) > limit {
		return fmt.Errorf("Hermes response exceeds %d MiB limit", limit>>20)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode Hermes response: %w", err)
	}
	return nil
}

func responseError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	result := &HTTPError{StatusCode: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Error) != 0 {
		var detail struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &detail) == nil {
			result.Code, result.Message = detail.Code, detail.Message
		} else {
			_ = json.Unmarshal(envelope.Error, &result.Message)
		}
	}
	if len(result.Message) > 2048 {
		result.Message = result.Message[:2048]
	}
	return result
}
