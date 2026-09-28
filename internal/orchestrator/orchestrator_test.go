package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	if len(o.Declarations()) != 12 {
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
