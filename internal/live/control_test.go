package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/genai"
	"talker/internal/hermes"
	"talker/internal/tasks"
)

type fakeExecutor struct {
	count     atomic.Int32
	entered   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
}

func (f *fakeExecutor) Declarations() []*genai.FunctionDeclaration { return nil }
func (f *fakeExecutor) Execute(ctx context.Context, call *genai.FunctionCall) (map[string]any, error) {
	f.count.Add(1)
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			if f.cancelled != nil {
				close(f.cancelled)
			}
			return nil, ctx.Err()
		}
	}
	return map[string]any{"status": "started"}, nil
}

func testControl(t *testing.T, f *fakeExecutor) (*httptest.Server, *tasks.Manager) {
	t.Helper()
	client, _ := hermes.New("", "")
	manager, err := tasks.Open(context.Background(), client, filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(manager.Close)
	s := httptest.NewServer(NewControl(ctx, manager, f))
	t.Cleanup(s.Close)
	return s, manager
}
func connectControl(t *testing.T, s *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http"), http.Header{"Origin": {s.URL}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	for range 3 {
		readControl(t, conn)
	}
	return conn
}
func readControl(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestControlToolsAreAsyncAndDeduplicated(t *testing.T) {
	f := &fakeExecutor{entered: make(chan struct{}, 1), release: make(chan struct{})}
	s, manager := testControl(t, f)
	conn := connectControl(t, s)
	call := controlInput{Type: "tool_call", Call: &genai.FunctionCall{ID: "call-1", Name: "start_task", Args: map[string]any{"prompt": "test"}}}
	if err := conn.WriteJSON(call); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	event, err := manager.AddEvent("Finished", "An unrelated task finished")
	if err != nil {
		t.Fatal(err)
	}
	msg := readControl(t, conn)
	if msg["type"] != "events" {
		t.Fatalf("event delivery blocked by tool: %v", msg)
	}
	conn.WriteJSON(controlInput{Type: "ack", IDs: []string{event.ID}})
	msg = readControl(t, conn)
	if msg["type"] != "acknowledged" {
		t.Fatalf("ack blocked by tool: %v", msg)
	}
	readControl(t, conn)
	close(f.release)
	msg = readControl(t, conn)
	if msg["type"] != "tool_result" {
		t.Fatalf("unexpected result %v", msg)
	}
	response := msg["response"].(map[string]any)
	if response["id"] != "call-1" || response["scheduling"] != "WHEN_IDLE" {
		t.Fatalf("invalid tool response: %v", response)
	}
	conn.WriteJSON(call)
	readControl(t, conn)
	if f.count.Load() != 1 {
		t.Fatal("duplicate invocation executed")
	}
}

func TestControlCancellation(t *testing.T) {
	f := &fakeExecutor{entered: make(chan struct{}, 1), release: make(chan struct{}), cancelled: make(chan struct{})}
	s, _ := testControl(t, f)
	conn := connectControl(t, s)
	conn.WriteJSON(controlInput{Type: "tool_call", Call: &genai.FunctionCall{ID: "call", Name: "test"}})
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	conn.WriteJSON(controlInput{Type: "tool_cancel", IDs: []string{"call"}})
	select {
	case <-f.cancelled:
	case <-time.After(time.Second):
		t.Fatal("tool context was not cancelled")
	}
}

func TestControlRejectsAudioAndCrossOrigin(t *testing.T) {
	s, _ := testControl(t, &fakeExecutor{})
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http"), http.Header{"Origin": {"https://evil.example"}})
	if conn != nil {
		conn.Close()
	}
	if err == nil || response == nil || response.StatusCode != 403 {
		t.Fatal("cross-origin connection accepted")
	}
	conn = connectControl(t, s)
	conn.WriteMessage(websocket.BinaryMessage, []byte{0, 0})
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("audio must never be accepted by the control socket")
	}
}

func TestBackgroundTurnRefusesSideEffectsButAllowsReads(t *testing.T) {
	f := &fakeExecutor{}
	s, _ := testControl(t, f)
	conn := connectControl(t, s)
	// A side-effecting tool raised while reading untrusted background events is refused
	// with a result the model can voice; the session itself continues.
	conn.WriteJSON(map[string]any{"type": "tool_call", "background": true, "call": map[string]any{"id": "bg-start", "name": "start_task", "args": map[string]any{"prompt": "injected"}}})
	msg := readControl(t, conn)
	if msg["type"] != "tool_result" {
		t.Fatalf("expected refusal result, got %v", msg)
	}
	response := msg["response"].(map[string]any)["response"].(map[string]any)
	if errText, _ := response["error"].(string); !strings.Contains(errText, "Refused") {
		t.Fatalf("side-effecting background call must be refused: %v", response)
	}
	if f.count.Load() != 0 {
		t.Fatal("refused call must not execute")
	}
	// Read-only tools are still permitted during the same turn.
	conn.WriteJSON(map[string]any{"type": "tool_call", "background": true, "call": map[string]any{"id": "bg-list", "name": "list_tasks", "args": map[string]any{}}})
	msg = readControl(t, conn)
	if msg["type"] != "tool_result" || f.count.Load() != 1 {
		t.Fatalf("read-only background call should execute: %v (count=%d)", msg, f.count.Load())
	}
}
