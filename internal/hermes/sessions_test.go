package hermes

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Response bodies mirror Hermes v0.21.4 as observed against a live gateway.
func sessionsServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer test-gateway-key-1234" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"Invalid gateway API key (API_SERVER_KEY)","type":"gateway_auth_error","code":"gateway_auth_failed"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/sessions":
			io.WriteString(w, `{"object":"list","data":[
			  {"id":"run_abc","title":"What voices are available\u2026","preview":"What voices are available for Gemini Live?","source":"api_server","model":"google/gemini-flash-latest","started_at":1790347717.79,"last_active":1790347725.1,"ended_at":null,"end_reason":null,"message_count":4,"tool_call_count":1,"archived":false,"pinned":false},
			  {"id":"20260926_011715_503961","title":"I am trying to setup another client\u2026","preview":"I am trying to setup...","source":"cli","model":"google/gemini-flash-latest","started_at":1790349487.08,"last_active":1790349499.47,"ended_at":1790349676.37,"end_reason":"cli_close","message_count":6,"tool_call_count":2,"archived":false,"pinned":false}
			],"limit":5,"offset":0,"has_more":true}`)
		case r.Method == "GET" && r.URL.Path == "/api/sessions/run_abc":
			io.WriteString(w, `{"object":"hermes.session","session":{"id":"run_abc","title":"What voices","source":"api_server","started_at":1790347717.79,"message_count":4}}`)
		case r.Method == "GET" && r.URL.Path == "/api/sessions/run_abc/messages":
			io.WriteString(w, `{"object":"list","session_id":"run_abc","pagination":{},"data":[
			  {"role":"user","content":"What voices are available?","timestamp":1790347717.8,"tool_calls":null,"reasoning":"hidden"},
			  {"role":"assistant","content":"","timestamp":1790347719.1,"tool_calls":[{"id":"call_1"}]},
			  {"role":"tool","content":"{\"success\":true}","timestamp":1790347719.3,"tool_call_id":"call_1","tool_name":"web_search"},
			  {"role":"assistant","content":"There are 30 voices, including Aoede and Puck.","timestamp":1790347725.0}
			]}`)
		case r.Method == "POST" && r.URL.Path == "/api/sessions":
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"title":"taken"`) {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"message":"Title already in use by session api_x","type":"invalid_request_error","code":"invalid_title"}}`)
				return
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"object":"hermes.session","session":{"id":"api_1790349962_e6deff79","source":"api_server","title":"untitled","started_at":1790349962.9,"message_count":0}}`)
		case r.Method == "POST" && r.URL.Path == "/api/sessions/api_1790349962_e6deff79/chat":
			io.WriteString(w, `{"object":"hermes.session.chat.completion","session_id":"api_1790349962_e6deff79","message":{"role":"assistant","content":"PONG"},"usage":{"input_tokens":29357,"output_tokens":2},"runtime":{"provider":"custom","model":"google/gemini-flash-latest"}}`)
		case r.Method == "GET" && r.URL.Path == "/health":
			io.WriteString(w, `{"status":"ok","platform":"hermes-agent","version":"0.21.4"}`)
		case r.Method == "GET" && r.URL.Path == "/api/model/options":
			io.WriteString(w, `{"model":"google/gemini-flash-latest","provider":"vertex-proxy","providers":[{"slug":"vertex-proxy","models":["google/gemini-flash-latest"]}]}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"message":"Session not found: `+r.URL.Path+`","type":"invalid_request_error","code":"session_not_found"}}`)
		}
	}))
	t.Cleanup(s.Close)
	return s, &seen
}

func TestSessionsListSendsRealQueryString(t *testing.T) {
	s, seen := sessionsServer(t)
	c, _ := New(s.URL, "test-gateway-key-1234")
	sessions, err := c.ListSessions(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	// The query must reach the server as a query, not as percent-encoded path.
	if (*seen)[0] != "GET /api/sessions?limit=5" {
		t.Fatalf("request line %q", (*seen)[0])
	}
	if len(sessions) != 2 || sessions[1].Source != "cli" || sessions[1].EndedAt == 0 || sessions[0].EndedAt != 0 {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
	if _, err := c.ListSessions(context.Background(), 0); err == nil {
		t.Fatal("limit 0 must be rejected before any request")
	}
}

func TestSessionMessagesKeepOnlySpokenTurns(t *testing.T) {
	s, _ := sessionsServer(t)
	c, _ := New(s.URL, "test-gateway-key-1234")
	msgs, err := c.SessionMessages(context.Background(), "run_abc", 40)
	if err != nil {
		t.Fatal(err)
	}
	// Tool results and empty tool-call turns are dropped; reasoning is never read.
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" || !strings.Contains(msgs[1].Content, "30 voices") {
		t.Fatalf("filtered messages: %+v", msgs)
	}
	if _, err := c.SessionMessages(context.Background(), "../etc", 10); err == nil {
		t.Fatal("path traversal in session ID must be rejected")
	}
	_, err = c.GetSession(context.Background(), "nope_missing")
	var httpErr *HTTPError
	if !asHTTP(err, &httpErr) || httpErr.StatusCode != 404 || httpErr.Code != "session_not_found" {
		t.Fatalf("missing session should surface Hermes's own error, got %v", err)
	}
}

func TestChatCreatesUntitledSessionAndReturnsAnswer(t *testing.T) {
	s, seen := sessionsServer(t)
	c, _ := New(s.URL, "test-gateway-key-1234")
	created, err := c.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := c.Chat(context.Background(), created.ID, "ping")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "PONG" || reply.SessionID != created.ID || reply.Model != "google/gemini-flash-latest" {
		t.Fatalf("reply %+v", reply)
	}
	if (*seen)[1] != "POST /api/sessions/api_1790349962_e6deff79/chat" {
		t.Fatalf("chat request %q", (*seen)[1])
	}
	// Hermes enforces unique titles; that error must propagate, not be masked.
	if _, err := c.CreateSession(context.Background(), "taken"); err == nil || !strings.Contains(err.Error(), "invalid_title") {
		t.Fatalf("duplicate title: %v", err)
	}
	if _, err := c.Chat(context.Background(), created.ID, "   "); err == nil {
		t.Fatal("blank input must be rejected locally")
	}
}

func TestHealthAndModelOptions(t *testing.T) {
	s, _ := sessionsServer(t)
	c, _ := New(s.URL, "test-gateway-key-1234")
	h, err := c.Health(context.Background())
	if err != nil || h.Status != "ok" || h.Version != "0.21.4" {
		t.Fatalf("health %+v %v", h, err)
	}
	m, err := c.ModelOptions(context.Background())
	if err != nil || m.Model != "google/gemini-flash-latest" || m.Provider != "vertex-proxy" {
		t.Fatalf("model options %+v %v", m, err)
	}
	wrong, _ := New(s.URL, "wrong-key-1234567890")
	_, err = wrong.Health(context.Background())
	var httpErr *HTTPError
	if !asHTTP(err, &httpErr) || httpErr.StatusCode != 401 || httpErr.Code != "gateway_auth_failed" {
		t.Fatalf("bad key must surface the gateway's 401, got %v", err)
	}
	disabled, _ := New("", "")
	if _, err := disabled.ListSessions(context.Background(), 5); err != ErrNotConfigured {
		t.Fatalf("disabled client: %v", err)
	}
}

func asHTTP(err error, target **HTTPError) bool { return errors.As(err, target) }
