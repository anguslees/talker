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

type askArgs struct {
	Question  string `json:"question" jsonschema:"A self-contained question or short request. Hermes may run its own tools (web search, files) before answering."`
	SessionID string `json:"session_id,omitempty" jsonschema:"Continue an existing Hermes session by ID; omit to ask in a fresh session."`
}

type sessionsArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"How many recent sessions to list, 1-30. Defaults to 10."`
}

type sessionArgs struct {
	SessionID string `json:"session_id" jsonschema:"Exact Hermes session ID from hermes_sessions. Runs started with start_task use their run ID."`
}

// sessionView is what the model hears about a Hermes conversation: enough to
// identify and summarise it, nothing about tokens or cost.
type sessionView struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Source      string `json:"source"`
	StartedAt   string `json:"started_at"`
	LastActive  string `json:"last_active"`
	Finished    bool   `json:"finished"`
	Messages    int    `json:"messages"`
	ToolCalls   int    `json:"tool_calls"`
	Model       string `json:"model,omitempty"`
	TrackedTask string `json:"tracked_task_id,omitempty"`
}

type messageView struct {
	Role string `json:"role"`
	Text string `json:"text"`
	At   string `json:"at"`
}

type answerView struct {
	Answer    string `json:"answer"`
	SessionID string `json:"session_id"`
	Model     string `json:"model,omitempty"`
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

func sessionToView(s hermes.Session, tracked map[string]string) sessionView {
	return sessionView{
		ID: s.ID, Title: limitText(firstNonEmpty(s.Title, s.Preview), 200), Source: s.Source,
		StartedAt: unixText(s.StartedAt), LastActive: unixText(s.LastActive),
		Finished: s.EndedAt != 0, Messages: s.MessageCount, ToolCalls: s.ToolCallCount,
		Model: s.Model, TrackedTask: tracked[s.ID],
	}
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
	trackedRuns := func() map[string]string {
		tracked := map[string]string{}
		for _, t := range manager.List() {
			if t.RunID != "" {
				tracked[t.RunID] = t.ID
			}
		}
		return tracked
	}
	mk := func(t tool.Tool, err error) built { return built{t, err} }
	return []built{
		mk(functiontool.New(functiontool.Config{Name: "ask_hermes", Description: "Ask Hermes a question and wait for the answer, typically a few seconds. Use for quick factual questions, lookups and short web searches where the user wants the answer now. For anything long-running or multi-step, use start_task instead so the user is not kept waiting."}, func(ctx agent.Context, a askArgs) (answerView, error) {
			sessionID := a.SessionID
			if sessionID == "" {
				// Hermes derives a title from the first message and rejects duplicates,
				// so a question-based title would collide on repeated questions.
				s, err := client.CreateSession(ctx, "")
				if err != nil {
					return answerView{}, err
				}
				sessionID = s.ID
			}
			reply, err := client.Chat(ctx, sessionID, a.Question)
			if err != nil {
				return answerView{}, err
			}
			return answerView{Answer: limitText(reply.Content, 12000), SessionID: sessionID, Model: reply.Model}, nil
		})),
		mk(functiontool.New(functiontool.Config{Name: "hermes_sessions", Description: "List recent Hermes conversations from every source (CLI, chat apps, this assistant), newest first. Use to answer 'what has Hermes been working on' or to find the session the user means. Read-only."}, func(ctx agent.Context, a sessionsArgs) (map[string]any, error) {
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
			tracked := trackedRuns()
			out := make([]sessionView, 0, len(sessions))
			for _, s := range sessions {
				if !s.Archived {
					out = append(out, sessionToView(s, tracked))
				}
			}
			return map[string]any{"sessions": out}, nil
		})),
		mk(functiontool.New(functiontool.Config{Name: "hermes_session", Description: "Read one Hermes session: what was asked and what Hermes concluded. Use after hermes_sessions to summarise it or answer questions about it. Read-only; reasoning and raw tool output are omitted."}, func(ctx agent.Context, a sessionArgs) (map[string]any, error) {
			session, err := client.GetSession(ctx, a.SessionID)
			if err != nil {
				return nil, err
			}
			messages, err := client.SessionMessages(ctx, a.SessionID, 40)
			if err != nil {
				return nil, err
			}
			out := make([]messageView, 0, len(messages))
			for _, m := range messages {
				out = append(out, messageView{Role: m.Role, Text: limitText(m.Content, 3000), At: unixText(m.Timestamp)})
			}
			return map[string]any{"session": sessionToView(session, trackedRuns()), "messages": out}, nil
		})),
		mk(functiontool.New(functiontool.Config{Name: "hermes_status", Description: "Check whether Hermes is reachable, its version, and which model it is currently routing to. Read-only."}, func(ctx agent.Context, _ struct{}) (statusView, error) {
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
