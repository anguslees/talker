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
		case r.Method == "GET" && (r.URL.Path == "/api/sessions/run_abc/messages" || r.URL.Path == "/api/sessions/run_compressed/messages"):
			if r.URL.Query().Get("order") != "latest" {
				// Hermes pages from the oldest row whenever a limit arrives without order=latest.
				io.WriteString(w, `{"object":"list","session_id":"run_abc","data":[{"id":1,"role":"user","content":"oldest","timestamp":1}]}`)
				return
			}
			// A compressed session reads from its live continuation, whose ID is reported.
			io.WriteString(w, `{"object":"list","session_id":"run_abc","pagination":{"order":"latest"},"data":[
			  {"id":53001,"role":"user","content":"What voices are available?","timestamp":1790347717.8,"tool_calls":null,"reasoning":"hidden","finish_reason":null},
			  {"id":53004,"role":"assistant","content":"","timestamp":1790347719.1,"tool_calls":[{"id":"call_1"}],"finish_reason":"tool_calls"},
			  {"id":53005,"role":"tool","content":"{\"success\":true}","timestamp":1790347719.3,"tool_call_id":"call_1","tool_name":"web_search","finish_reason":null},
			  {"id":53009,"role":"assistant","content":"There are 30 voices, including Aoede and Puck.","timestamp":1790347725.0,"finish_reason":"stop"},
			  {"id":53010,"role":"user","content":[{"type":"text","text":"And this image?"},{"type":"image_url","image_url":{"url":"data:"}}],"timestamp":1790347730.0}
			]}`)
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

func TestSessionMessagesPageFromNewestAndFollowCompression(t *testing.T) {
	s, seen := sessionsServer(t)
	c, _ := New(s.URL, "test-gateway-key-1234")
	page, err := c.SessionMessages(context.Background(), "run_compressed", 40, 20)
	if err != nil {
		t.Fatal(err)
	}
	if (*seen)[0] != "GET /api/sessions/run_compressed/messages?order=latest&limit=40&offset=20" {
		t.Fatalf("request line %q", (*seen)[0])
	}
	if page.SessionID != "run_abc" || len(page.Messages) != 5 {
		t.Fatalf("page = %+v", page)
	}
	stop, image := page.Messages[3], page.Messages[4]
	if stop.ID != 53009 || stop.FinishReason != "stop" || page.Messages[1].FinishReason != "tool_calls" {
		t.Fatalf("turn markers lost: %+v", page.Messages)
	}
	if image.Content != "And this image?" {
		t.Fatalf("multimodal content = %q", image.Content)
	}
	// Tool results and empty tool-call turns are dropped; reasoning is never read.
	spoken := Conversational(page.Messages)
	if len(spoken) != 3 || spoken[0].Role != "user" || !strings.Contains(spoken[1].Content, "30 voices") {
		t.Fatalf("filtered messages: %+v", spoken)
	}
	for _, bad := range [][2]int{{0, 0}, {501, 0}, {10, -1}} {
		if _, err := c.SessionMessages(context.Background(), "run_abc", bad[0], bad[1]); err == nil {
			t.Fatalf("page %v must be rejected before any request", bad)
		}
	}
	if _, err := c.SessionMessages(context.Background(), "../etc", 10, 0); err == nil {
		t.Fatal("path traversal in session ID must be rejected")
	}
	_, err = c.GetSession(context.Background(), "nope_missing")
	var httpErr *HTTPError
	if !asHTTP(err, &httpErr) || httpErr.StatusCode != 404 || httpErr.Code != "session_not_found" {
		t.Fatalf("missing session should surface Hermes's own error, got %v", err)
	}
}

func TestConversationBoundaries(t *testing.T) {
	for reason, boundary := range map[string]bool{"new_session": true, "session_reset": true, "branched": true, "compression": false, "agent_close": false, "cli_close": false, "": false} {
		if IsConversationBoundary(reason) != boundary {
			t.Errorf("IsConversationBoundary(%q) = %t", reason, !boundary)
		}
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
