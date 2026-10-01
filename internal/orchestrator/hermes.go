package orchestrator

import (
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/anguslees/talker/internal/hermes"
	"github.com/anguslees/talker/internal/tasks"
)

// Hermes-side tools operate on the gateway's own state (conversations from every
// source, the current model, health), as opposed to the task tools, which operate
// on Talker's durable ledger of runs it is monitoring.

type built struct {
	tool tool.Tool
	err  error
}

func wrap(t tool.Tool, err error) built { return built{t, err} }

// askWait bounds how long ask_hermes holds its tool call, inside the control
// socket's 30 s tool-call limit.
const askWait = 20 * time.Second

type askArgs struct {
	Question  string `json:"question" jsonschema:"A self-contained question or short request. Hermes may run its own tools (web search, files) before answering."`
	SessionID string `json:"session_id,omitempty" jsonschema:"A different Hermes session ID to continue; omit to ask in the current conversation."`
}

type sessionsArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"How many recent sessions to list, 1-30. Defaults to 10."`
}

type sessionArgs struct {
	SessionID string `json:"session_id" jsonschema:"Exact Hermes session ID from hermes_sessions, or a task's session_id from list_tasks."`
}

// sessionView is what the model hears about a Hermes conversation: enough to
// identify and summarise it, nothing about tokens or cost.
type sessionView struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Source       string `json:"source"`
	StartedAt    string `json:"started_at"`
	LastActive   string `json:"last_active,omitempty"`
	Finished     bool   `json:"finished"`
	Messages     int    `json:"messages"`
	ToolCalls    int    `json:"tool_calls"`
	Model        string `json:"model,omitempty"`
	TrackedTask  string `json:"tracked_task_id,omitempty"`
	Following    bool   `json:"following,omitempty"`
	Conversation bool   `json:"current_conversation,omitempty"`
}

type messageView struct {
	Role string `json:"role"`
	Text string `json:"text"`
	At   string `json:"at"`
}

type statusView struct {
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	Model     string `json:"current_model,omitempty"`
	Provider  string `json:"current_provider,omitempty"`
	Problem   string `json:"problem,omitempty"`
}

func unixText(seconds float64) string {
	if seconds == 0 {
		return ""
	}
	return time.Unix(int64(seconds), 0).UTC().Format(time.RFC3339)
}

func sessionToView(s hermes.Session, known sessionMarks) sessionView {
	return sessionView{
		ID: s.ID, Title: limitText(firstNonEmpty(s.Title, s.Preview), 200), Source: s.Source,
		StartedAt: unixText(s.StartedAt), LastActive: unixText(s.LastActive),
		Finished: s.EndedAt != 0, Messages: s.MessageCount, ToolCalls: s.ToolCallCount,
		Model: s.Model, TrackedTask: known.tracked[s.ID], Following: known.following[s.ID], Conversation: s.ID == known.conversation,
	}
}

// sessionMarks relates Hermes sessions to Talker's own state.
type sessionMarks struct {
	tracked      map[string]string
	following    map[string]bool
	conversation string
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func hermesTools(manager *tasks.Manager) []built {
	client := manager.Client()
	marks := func() sessionMarks {
		m := sessionMarks{tracked: map[string]string{}, following: map[string]bool{}}
		running := map[string]bool{}
		// A session names its running task, else its most recent one.
		for _, t := range manager.List() {
			session := t.HermesSession()
			if session == "" || running[session] {
				continue
			}
			if _, seen := m.tracked[session]; !seen || !t.Finished() {
				m.tracked[session], running[session] = t.ID, !t.Finished()
			}
		}
		for _, f := range manager.Follows() {
			m.following[f.SessionID] = true
		}
		if c, ok := manager.CurrentConversation(); ok {
			m.conversation = c.SessionID
		}
		return m
	}
	return []built{
		wrap(functiontool.New(functiontool.Config{Name: "ask_hermes", Description: "Ask Hermes a question in the current conversation and wait up to about 20 seconds for the answer. Use for quick factual questions, lookups and short web searches where the user wants the answer now. If Hermes needs longer, the question continues as a background task and is announced when done. For anything long-running or multi-step, use start_task instead."}, func(ctx agent.Context, a askArgs) (taskView, error) {
			task, done, err := manager.Ask(ctx, a.Question, a.SessionID, askWait)
			if task.ID == "" {
				return taskView{}, err
			}
			v := startedView(task, 12000)
			switch {
			case err != nil:
				v.Note = strings.TrimSpace(v.Note + " This task is tracked; inspect its status rather than asking again: " + err.Error())
			case !done:
				v.Note = strings.TrimSpace(v.Note + " Hermes is still working on this; it continues as a background task and is announced when done. Do not ask again.")
			case task.Status == "waiting_for_approval":
				// The request reaches the user as an announcement, like any task's.
				v.Approval = nil
				v.Note = strings.TrimSpace(v.Note + " Hermes needs an approval before it can answer; the request is announced separately.")
			}
			return v, nil
		})),
		wrap(functiontool.New(functiontool.Config{Name: "hermes_sessions", Description: "List recent Hermes conversations from every source (CLI, web UI, chat apps, this assistant), newest first. Use to answer 'what has Hermes been working on' or to find the session the user means. Read-only."}, func(ctx agent.Context, a sessionsArgs) (map[string]any, error) {
			limit := a.Limit
			if limit <= 0 {
				limit = 10
			}
			if limit > 30 {
				limit = 30
			}
			sessions, err := client.ListSessions(ctx, limit)
			if err != nil {
				return nil, err
			}
			known := marks()
			out := make([]sessionView, 0, len(sessions))
			for _, s := range sessions {
				if !s.Archived {
					out = append(out, sessionToView(s, known))
				}
			}
			return map[string]any{"sessions": out}, nil
		})),
		wrap(functiontool.New(functiontool.Config{Name: "hermes_session", Description: "Read the latest exchanges of one Hermes session: what was asked and what Hermes concluded. Use after hermes_sessions to summarise it or answer questions about it. Read-only; reasoning and raw tool output are omitted."}, func(ctx agent.Context, a sessionArgs) (map[string]any, error) {
			page, err := client.SessionMessages(ctx, a.SessionID, 100, 0)
			if err != nil {
				return nil, err
			}
			// A compressed session continues under a new ID, which page reports.
			session, err := client.GetSession(ctx, page.SessionID)
			if err != nil {
				return nil, err
			}
			messages := hermes.Conversational(page.Messages)
			messages = messages[max(len(messages)-40, 0):]
			out := make([]messageView, 0, len(messages))
			for _, m := range messages {
				out = append(out, messageView{Role: m.Role, Text: limitText(m.Content, 3000), At: unixText(m.Timestamp)})
			}
			return map[string]any{"session": sessionToView(session, marks()), "messages": out}, nil
		})),
		wrap(functiontool.New(functiontool.Config{Name: "hermes_status", Description: "Check whether Hermes is reachable, its version, and which model it is currently routing to. Read-only."}, func(ctx agent.Context, _ struct{}) (statusView, error) {
			health, err := client.Health(ctx)
			if err != nil {
				return statusView{Reachable: false, Problem: limitText(err.Error(), 300)}, nil
			}
			view := statusView{Reachable: health.Status == "ok", Version: health.Version}
			if options, err := client.ModelOptions(ctx); err == nil {
				view.Model, view.Provider = options.Model, options.Provider
			}
			return view, nil
		})),
	}
}
