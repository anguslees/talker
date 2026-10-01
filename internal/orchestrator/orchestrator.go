package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/anguslees/talker/internal/tasks"
)

type callable interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(agent.Context, any) (map[string]any, error)
}

// Orchestrator runs Live-selected tools through ADK without a second LLM round trip.
type Orchestrator struct {
	runner       *runner.Runner
	sessions     session.Service
	tools        map[string]callable
	declarations []*genai.FunctionDeclaration
}

type startArgs struct {
	Prompt    string `json:"prompt" jsonschema:"Complete task or question for Hermes, including relevant conversation context. Ask for sources when searching the web."`
	SessionID string `json:"session_id,omitempty" jsonschema:"A different Hermes session ID to continue; omit to continue the current conversation."`
}

type idArgs struct {
	ID string `json:"id" jsonschema:"The exact Talker task ID returned by start_task or list_tasks."`
}

type steerArgs struct {
	ID    string `json:"id"`
	Input string `json:"input" jsonschema:"The user's additional instructions for this running task."`
}

type approvalArgs struct {
	ID        string `json:"id"`
	RequestID string `json:"request_id" jsonschema:"Exact pending Hermes approval request ID."`
	Choice    string `json:"choice" jsonschema:"One of the advertised choices, explicitly selected by the user after hearing the requested action."`
}

type ackArgs struct {
	IDs []string `json:"ids"`
}

type watchArgs struct {
	RunID string `json:"run_id" jsonschema:"The exact Hermes run ID, as reported by Hermes or the user."`
}

// taskView is the JSON-native, bounded projection returned to the model. It
// omits PendingSteer, whose free-form JSON defeats ADK's inferred output schema.
type taskView struct {
	ID           string        `json:"id"`
	RunID        string        `json:"run_id,omitempty"`
	SessionID    string        `json:"session_id,omitempty"`
	Prompt       string        `json:"prompt"`
	Status       string        `json:"status"`
	Output       string        `json:"output,omitempty"`
	Error        string        `json:"error,omitempty"`
	CreatedAt    string        `json:"created_at"`
	UpdatedAt    string        `json:"updated_at"`
	Approval     *approvalView `json:"approval,omitempty"`
	Conversation string        `json:"conversation,omitempty"`
	Note         string        `json:"note,omitempty"`
}

type approvalView struct {
	RequestID   string   `json:"request_id"`
	Command     string   `json:"command,omitempty"`
	Description string   `json:"description,omitempty"`
	Choices     []string `json:"choices,omitempty"`
}

// Notes explaining a request that could not continue the conversation.
const (
	separateNote         = "A task of yours is running in the current conversation, so this started in a separate Hermes session without the conversation's context. If it adjusts that running task, steer it instead."
	separateExternalNote = "The user has a turn running in the current conversation from another Hermes client, so this started in a separate Hermes session without the conversation's context. That turn cannot be steered from here."
)

func view(t tasks.Task, outputLimit int) taskView {
	v := taskView{
		ID: t.ID, RunID: t.RunID, SessionID: t.SessionID, Status: t.Status, Conversation: t.Conversation,
		Prompt:    limitText(t.Prompt, 4000),
		Output:    limitText(t.Output, outputLimit),
		Error:     limitText(t.Error, 4000),
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if t.Approval != nil {
		v.Approval = &approvalView{
			RequestID:   t.Approval.RequestID,
			Command:     limitText(t.Approval.Command, 4000),
			Description: limitText(t.Approval.Description, 2000),
			Choices:     t.Approval.Choices,
		}
	}
	return v
}

// startedView is the result of submitting work, explaining a request that
// could not continue the conversation.
func startedView(t tasks.Task, outputLimit int) taskView {
	v := view(t, outputLimit)
	switch t.Conversation {
	case tasks.RouteSeparate:
		v.Note = separateNote
	case tasks.RouteSeparateExternal:
		v.Note = separateExternalNote
	}
	return v
}

func New(manager *tasks.Manager) (*Orchestrator, error) {
	o := &Orchestrator{sessions: session.InMemoryService(), tools: make(map[string]callable)}
	var buildErr error
	add := func(t tool.Tool, err error) {
		if err != nil {
			buildErr = errors.Join(buildErr, err)
			return
		}
		f := t.(callable)
		o.tools[f.Name()] = f
		d := f.Declaration()
		d.Behavior = genai.BehaviorNonBlocking
		d.ResponseJsonSchema = nil
		o.declarations = append(o.declarations, d)
	}
	add(functiontool.New(functiontool.Config{Name: "start_task", Description: "Start a Hermes task, question, or web search in the background. It continues the current Hermes conversation unless given another session. Returns admission, not the final answer. Completion is automatically announced during a quiet break; never submit a duplicate just to check progress."}, func(ctx agent.Context, a startArgs) (taskView, error) {
		task, err := manager.Start(ctx, a.Prompt, a.SessionID)
		if err != nil {
			// Only a persisted record is genuinely tracked; a failed first write is a real failure.
			if tracked, ok := manager.Get(task.ID); ok && task.ID != "" {
				v := startedView(tracked, 4000)
				v.Note = strings.TrimSpace(v.Note + " This task is tracked; inspect its status rather than submitting again: " + err.Error())
				return v, nil
			}
			return taskView{}, err
		}
		return startedView(task, 4000), nil
	}))
	add(functiontool.New(functiontool.Config{Name: "list_tasks", Description: "List Talker's tracked tasks, results, errors and pending approvals, the current Hermes conversation, and followed sessions. Use to identify 'that task'; never invent IDs."}, func(ctx agent.Context, a struct{}) (map[string]any, error) {
		result := map[string]any{"tasks": manager.Recent(20), "following": followViews(manager.Follows())}
		if c, ok := manager.CurrentConversation(); ok {
			result["conversation"] = conversationView{SessionID: c.SessionID, StartedAt: c.StartedAt.UTC().Format(time.RFC3339), ActiveTask: c.ActiveTask}
		}
		return result, nil
	}))
	add(functiontool.New(functiontool.Config{Name: "watch_task", Description: "Monitor an existing Hermes run started outside Talker and announce its completion. Requires its exact Hermes run ID; do not invent an ID or create replacement work."}, func(ctx agent.Context, a watchArgs) (taskView, error) {
		t, err := manager.Watch(ctx, a.RunID)
		if err != nil {
			return taskView{}, err
		}
		return view(t, 4000), nil
	}))
	add(functiontool.New(functiontool.Config{Name: "get_task", Description: "Read a tracked task's current status and result. All started tasks already have automatic completion notifications."}, func(ctx agent.Context, a idArgs) (taskView, error) {
		t, ok := manager.Get(a.ID)
		if !ok {
			return taskView{}, errors.New("unknown task ID")
		}
		return view(t, 24000), nil
	}))
	add(functiontool.New(functiontool.Config{Name: "stop_task", Description: "Request Hermes to stop a task ONLY when the user asks. Barge-in or changing conversational topic does not mean cancel background work. This cannot undo completed actions."}, func(ctx agent.Context, a idArgs) (taskView, error) {
		t, err := manager.Stop(ctx, a.ID)
		if err != nil {
			return taskView{}, err
		}
		return view(t, 4000), nil
	}))
	add(functiontool.New(functiontool.Config{Name: "steer_task", Description: "Queue the user's new instructions for a running task. Queued guidance is not guaranteed to be consumed before completion."}, func(ctx agent.Context, a steerArgs) (map[string]any, error) {
		err := manager.Steer(ctx, a.ID, a.Input)
		return map[string]any{"queued": err == nil}, err
	}))
	add(functiontool.New(functiontool.Config{Name: "answer_approval", Description: "Answer an exact pending Hermes tool approval ONLY after describing the action and receiving the user's explicit choice. Never choose session/always permission on the user's behalf."}, func(ctx agent.Context, a approvalArgs) (map[string]any, error) {
		err := manager.Approve(ctx, a.ID, a.RequestID, a.Choice)
		return map[string]any{"submitted": err == nil}, err
	}))
	add(functiontool.New(functiontool.Config{Name: "acknowledge_events", Description: "Dismiss pending notifications when the user says they heard them or asks to dismiss them. Use exact event IDs."}, func(ctx agent.Context, a ackArgs) (map[string]any, error) {
		err := manager.Ack(a.IDs)
		return map[string]any{"acknowledged": err == nil}, err
	}))
	for _, t := range append(hermesTools(manager), conversationTools(manager)...) {
		add(t.tool, t.err)
	}
	if buildErr != nil {
		return nil, buildErr
	}
	a, err := agent.New(agent.Config{Name: "talker_tools", Description: "Execute the voice agent's selected tools with typed validation and ADK invocation context.", Run: o.run})
	if err != nil {
		return nil, err
	}
	o.runner, err = runner.New(runner.Config{AppName: "talker", Agent: a, SessionService: o.sessions})
	return o, err
}

func limitText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + " [truncated]"
}

func (o *Orchestrator) Declarations() []*genai.FunctionDeclaration { return o.declarations }

func (o *Orchestrator) run(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		var call genai.FunctionCall
		content := ctx.UserContent()
		if content == nil || len(content.Parts) != 1 {
			yield(nil, errors.New("expected a tool invocation"))
			return
		}
		if err := json.Unmarshal([]byte(content.Parts[0].Text), &call); err != nil {
			yield(nil, err)
			return
		}
		t, ok := o.tools[call.Name]
		if !ok {
			yield(nil, fmt.Errorf("unknown tool %q", call.Name))
			return
		}
		if call.Args == nil {
			call.Args = map[string]any{}
		}
		result, err := t.Run(agent.NewToolContext(ctx, call.ID, nil, nil), call.Args)
		if err != nil {
			yield(nil, err)
			return
		}
		e := session.NewEvent(ctx, ctx.InvocationID())
		e.LLMResponse = model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result}}}}}
		yield(e, nil)
	}
}

func (o *Orchestrator) Execute(ctx context.Context, call *genai.FunctionCall) (map[string]any, error) {
	s, err := o.sessions.Create(ctx, &session.CreateRequest{AppName: "talker", UserID: "owner"})
	if err != nil {
		return nil, err
	}
	defer o.sessions.Delete(context.WithoutCancel(ctx), &session.DeleteRequest{AppName: "talker", UserID: "owner", SessionID: s.Session.ID()})
	data, err := json.Marshal(call)
	if err != nil {
		return nil, err
	}
	for event, err := range o.runner.Run(ctx, "owner", s.Session.ID(), genai.NewContentFromText(string(data), genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			return nil, err
		}
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part.FunctionResponse != nil {
				return part.FunctionResponse.Response, nil
			}
		}
	}
	return nil, errors.New("tool invocation ended without a result")
}
