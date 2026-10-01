package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anguslees/talker/internal/hermes"
	"github.com/anguslees/talker/internal/tasks"
	"google.golang.org/genai"
)

func TestADKDispatchAndValidation(t *testing.T) {
	client, err := hermes.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.Open(context.Background(), client, filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	o, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Declarations()) != 15 {
		t.Fatalf("got %d tools", len(o.Declarations()))
	}
	for _, decl := range o.Declarations() {
		if decl.Behavior != genai.BehaviorNonBlocking {
			t.Fatalf("blocking declaration %s", decl.Name)
		}
	}
	result, err := o.Execute(context.Background(), &genai.FunctionCall{ID: "list", Name: "list_tasks", Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result["tasks"]; !ok {
		t.Fatalf("missing result: %#v", result)
	}
	for _, call := range []*genai.FunctionCall{
		{ID: "unknown", Name: "not_a_tool"},
		{ID: "invalid", Name: "start_task", Args: map[string]any{"prompt": 123}},
		{ID: "missing", Name: "get_task", Args: map[string]any{}},
	} {
		if _, err := o.Execute(context.Background(), call); err == nil {
			t.Fatalf("expected error for %+v", call)
		}
	}
	if _, err := o.Execute(context.Background(), &genai.FunctionCall{ID: "disabled", Name: "start_task", Args: map[string]any{"prompt": "test"}}); err == nil {
		t.Fatal("unconfigured Hermes must not report success")
	}
}

// A task carrying free-form pending_steer JSON, an approval, and a large output
// must still round-trip through ADK's output validation and stay bounded.
func TestTaskResultsAreJSONNativeAndBounded(t *testing.T) {
	large := strings.Repeat("x", 100_000)
	hermesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/runs/run_existing" && r.Method == http.MethodGet:
			io.WriteString(w, `{"object":"hermes.run","run_id":"run_existing","session_id":"s1","status":"waiting_for_approval",
				"output":"`+large+`","pending_steer":{"input":"focus on AU sources","queued_at":1.5},
				"approval":{"request_id":"apr_1","command":"rm -rf build","description":"`+large+`","choices":["once","deny"]}}`)
		case r.URL.Path == "/v1/runs" && r.Method == http.MethodPost:
			t.Error("watch must never create a run")
		default:
			http.NotFound(w, r)
		}
	}))
	defer hermesServer.Close()
	client, err := hermes.New(hermesServer.URL, "test-key-1234567890")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.Open(context.Background(), client, filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	o, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}
	watched, err := o.Execute(context.Background(), &genai.FunctionCall{ID: "w", Name: "watch_task", Args: map[string]any{"run_id": "run_existing"}})
	if err != nil {
		t.Fatalf("watch_task through ADK: %v", err)
	}
	id, _ := watched["id"].(string)
	if id == "" {
		t.Fatalf("watch result lacks task id: %#v", watched)
	}
	got, err := o.Execute(context.Background(), &genai.FunctionCall{ID: "g", Name: "get_task", Args: map[string]any{"id": id}})
	if err != nil {
		t.Fatalf("get_task through ADK: %v", err)
	}
	encoded, _ := json.Marshal(got)
	if len(encoded) > 64<<10 {
		t.Fatalf("get_task result unbounded: %d bytes", len(encoded))
	}
	if _, present := got["pending_steer"]; present {
		t.Fatal("free-form pending_steer must not reach the model")
	}
	approval, _ := got["approval"].(map[string]any)
	if approval == nil || approval["request_id"] != "apr_1" {
		t.Fatalf("approval projection missing: %#v", got)
	}
	if desc, _ := approval["description"].(string); !strings.HasSuffix(desc, "[truncated]") {
		t.Fatal("approval description must be bounded")
	}
}

// Conversation and session tools must round-trip through ADK's inferred
// output schemas; ask_hermes answers through a tracked run, never /chat.
func TestConversationToolsThroughADK(t *testing.T) {
	var posted []map[string]any
	runSessions := map[string]string{}
	var mu sync.Mutex
	hermesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/capabilities":
			io.WriteString(w, `{"object":"hermes.api_server.capabilities","features":{"run_submission":true,"run_status":true,"runs_idempotency":{"supported":true,"durable":true,"retention_seconds":86400}}}`)
		case r.URL.Path == "/v1/runs" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			posted = append(posted, body)
			id := fmt.Sprintf("run_%d", len(posted))
			runSessions[id], _ = body["session_id"].(string)
			if runSessions[id] == "" {
				runSessions[id] = id
			}
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `{"run_id":%q,"status":"started"}`, id)
		case strings.HasPrefix(r.URL.Path, "/v1/runs/") && strings.HasSuffix(r.URL.Path, "/events"):
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/v1/runs/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
			mu.Lock()
			session := runSessions[id]
			mu.Unlock()
			fmt.Fprintf(w, `{"object":"hermes.run","run_id":%q,"session_id":%q,"status":"completed","output":"Canberra."}`, id, session)
		case r.URL.Path == "/api/sessions/webui_1/messages":
			if r.URL.Query().Get("order") != "latest" {
				t.Error("session messages must be read newest first")
			}
			io.WriteString(w, `{"object":"list","session_id":"webui_1","data":[
				{"id":7,"role":"user","content":"Fix harder","timestamp":1790561834,"finish_reason":null},
				{"id":9,"role":"assistant","content":"Root cause found.","timestamp":1790562969,"finish_reason":"stop"}]}`)
		case r.URL.Path == "/api/sessions/webui_1":
			io.WriteString(w, `{"object":"hermes.session","session":{"id":"webui_1","title":"Cron fix","source":"webui","message_count":2}}`)
		case r.URL.Path == "/api/sessions/run_1":
			io.WriteString(w, `{"object":"hermes.session","session":{"id":"run_1","message_count":0}}`)
		case r.URL.Path == "/api/sessions/run_1/messages":
			io.WriteString(w, `{"object":"list","session_id":"run_1","data":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer hermesServer.Close()
	client, err := hermes.New(hermesServer.URL, "test-key-1234567890")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.Open(context.Background(), client, filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	o, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		result, err := o.Execute(context.Background(), &genai.FunctionCall{ID: name, Name: name, Args: args})
		if err != nil {
			t.Fatalf("%s through ADK: %v", name, err)
		}
		return result
	}
	answer := call("ask_hermes", map[string]any{"question": "Capital of Australia?"})
	if answer["output"] != "Canberra." || answer["status"] != "completed" || answer["conversation"] != tasks.RouteStarted {
		t.Fatalf("ask_hermes = %#v", answer)
	}
	listed := call("list_tasks", map[string]any{})
	if conv, _ := listed["conversation"].(map[string]any); conv == nil || conv["session_id"] != "run_1" {
		t.Fatalf("list_tasks conversation = %#v", listed["conversation"])
	}
	session := call("hermes_session", map[string]any{"session_id": "webui_1"})
	if messages, _ := session["messages"].([]any); len(messages) != 2 {
		t.Fatalf("hermes_session = %#v", session)
	}
	followed := call("follow_session", map[string]any{"session_id": "webui_1"})
	if followed["session_id"] != "webui_1" || followed["title"] != "Cron fix" {
		t.Fatalf("follow_session = %#v", followed)
	}
	continued := call("start_task", map[string]any{"prompt": "Check the fix held", "session_id": "webui_1"})
	if continued["session_id"] != "webui_1" || continued["conversation"] != tasks.RouteContinued {
		t.Fatalf("start_task in webui_1 = %#v", continued)
	}
	// A continued task is tracked under the session it writes to, not its run ID.
	session = call("hermes_session", map[string]any{"session_id": "webui_1"})
	if view, _ := session["session"].(map[string]any); view["tracked_task_id"] != continued["id"] || view["current_conversation"] != true {
		t.Fatalf("hermes_session marks = %#v", session["session"])
	}
	if got := call("unfollow_session", map[string]any{"session_id": "webui_1"}); got["unfollowed"] != true {
		t.Fatalf("unfollow_session = %#v", got)
	}
	if got := call("new_conversation", map[string]any{}); got["ended_session_id"] != "webui_1" {
		t.Fatalf("new_conversation = %#v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 2 || posted[0]["session_id"] != nil || posted[1]["session_id"] != "webui_1" {
		t.Fatalf("posted runs = %#v", posted)
	}
}

func TestSeparateNotesNameTheirCause(t *testing.T) {
	for route, want := range map[string]string{tasks.RouteSeparate: "steer it", tasks.RouteSeparateExternal: "cannot be steered", tasks.RouteContinued: ""} {
		note := startedView(tasks.Task{Conversation: route}, 100).Note
		if (want == "") != (note == "") || !strings.Contains(note, want) {
			t.Errorf("%s note = %q", route, note)
		}
	}
}
