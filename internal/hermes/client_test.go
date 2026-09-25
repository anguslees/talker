package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRoutesAndSchemas(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("authorization = %q", got)
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /p/voice/v1/capabilities":
			fmt.Fprint(w, `{"object":"hermes.api_server.capabilities","features":{"run_submission":true,"run_status":true,"runs_idempotency":{"supported":true,"durable":true,"retention_seconds":86400}}}`)
		case "POST /p/voice/v1/runs":
			if r.Header.Get("Idempotency-Key") != "stable-key" {
				t.Error("missing stable idempotency key")
			}
			var input RunRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input != (RunRequest{Input: "search the web", SessionID: "voice-session"}) {
				t.Errorf("input = %+v, error = %v", input, err)
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"run_id":"run_1","status":"started","replayed":false}`)
		case "GET /p/voice/v1/runs/run_1":
			fmt.Fprint(w, `{"object":"hermes.run","run_id":"run_1","status":"completed","output":"answer","session_id":"voice-session","usage":{"input_tokens":5,"output_tokens":10,"total_tokens":15}}`)
		case "POST /p/voice/v1/runs/run_1/stop":
			fmt.Fprint(w, `{"run_id":"run_1","status":"stopping"}`)
		case "POST /p/voice/v1/runs/run_1/steer":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["input"] != "use citations" {
				t.Errorf("steer body = %#v", body)
			}
			fmt.Fprint(w, `{"object":"hermes.run.steer","run_id":"run_1","accepted":true}`)
		case "POST /p/voice/v1/runs/run_1/approval":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !reflect.DeepEqual(body, map[string]string{"request_id": "approval-1", "choice": "deny"}) {
				t.Errorf("approval body = %#v", body)
			}
			fmt.Fprint(w, `{"object":"hermes.run.approval_response","run_id":"run_1","resolved":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(server.URL+"/p/voice/", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	caps, err := client.Capabilities(ctx)
	if err != nil || !caps.Features.RunsIdempotency.Durable {
		t.Fatalf("capabilities = %+v, %v", caps, err)
	}
	admission, err := client.CreateRun(ctx, RunRequest{Input: "search the web", SessionID: "voice-session"}, "stable-key")
	if err != nil || admission.RunID != "run_1" {
		t.Fatalf("admission = %+v, %v", admission, err)
	}
	run, err := client.GetRun(ctx, admission.RunID)
	if err != nil || run.Output != "answer" || run.Usage.TotalTokens != 15 {
		t.Fatalf("run = %+v, %v", run, err)
	}
	if _, err := client.StopRun(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}
	if err := client.SteerRun(ctx, run.RunID, "use citations"); err != nil {
		t.Fatal(err)
	}
	if err := client.ApproveRun(ctx, run.RunID, "approval-1", "deny"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 6 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestDisabledAndInvalidConfiguration(t *testing.T) {
	client, err := New("", "")
	if err != nil || client.Configured() {
		t.Fatalf("disabled client = %+v, %v", client, err)
	}
	if _, err := client.Capabilities(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("error = %v", err)
	}
	for _, base := range []string{"ftp://example.com", "http://u:p@example.com", "http://example.com/?key=secret", "http://example.com/#fragment", "/relative"} {
		if _, err := New(base, "key"); err == nil {
			t.Errorf("accepted URL %q", base)
		}
	}
	if _, err := New("http://example.com", ""); err == nil || !strings.Contains(err.Error(), "HERMES_API_KEY") {
		t.Fatalf("missing key must name the missing variable: %v", err)
	}
	if _, err := New("http://example.com", "key\r\nother"); err == nil {
		t.Fatal("accepted header injection")
	}
}

func TestNoRedirectAndStructuredErrors(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "capabilities") {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":{"code":"idempotency_key_conflict","message":"different payload"}}`)
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret")
	if _, err := client.Capabilities(context.Background()); err == nil || reached.Load() {
		t.Fatalf("redirect followed or no error: %v", err)
	}
	_, err := client.CreateRun(context.Background(), RunRequest{Input: "hello"}, "key")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != "idempotency_key_conflict" || Retryable(err) {
		t.Fatalf("conflict = %v", err)
	}
}

func TestBoundedResponseAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runs/run_slow" {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, strings.Repeat(" ", maxResponseBytes+1))
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret")
	if _, err := client.Capabilities(context.Background()); err == nil || !strings.Contains(err.Error(), "size") && !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversize response: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := client.GetRun(ctx, "run_slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestSSEWireFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p/voice/v1/runs/run_1/events" || r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("Last-Event-ID") != "4" {
			t.Errorf("incorrect SSE request: %s %#v", r.URL.Path, r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keepalive\n\nid: 5\ndata: {\"event\":\"message.delta\",\"run_id\":\"run_1\",\n")
		fmt.Fprint(w, "data: \"delta\":\"hello\",\"seq\":5}\n\n")
		fmt.Fprint(w, "data: {\"event\":\"tool.completed\",\"run_id\":\"run_1\",\"error\":false}\n\n")
		fmt.Fprint(w, "id: 6\ndata: {\"event\":\"run.completed\",\"run_id\":\"run_1\",\"output\":\"hello\"}\n\n: stream closed\n\n")
	}))
	defer server.Close()
	client, _ := New(server.URL+"/p/voice", "key")
	var events []RunEvent
	err := client.StreamEvents(context.Background(), "run_1", "4", func(event RunEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil || len(events) != 3 || events[0].Delta != "hello" || events[0].ID != "5" || events[2].Event != "run.completed" {
		t.Fatalf("events = %+v, error = %v", events, err)
	}
}

func TestSSEBoundsAndIncompleteFrames(t *testing.T) {
	for name, body := range map[string]string{
		"line":       "data: " + strings.Repeat("x", maxSSELineBytes) + "\n\n",
		"event":      strings.Repeat("data: "+strings.Repeat("x", 32000)+"\n", 10) + "\n",
		"incomplete": "data: {\"event\":\"run.completed\"}\n",
		"invalid":    "data: not-json\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := parseSSE(strings.NewReader(body), func() {}, func(RunEvent) error { return nil }); err == nil {
				t.Fatal("accepted invalid or oversized event")
			}
		})
	}
}
