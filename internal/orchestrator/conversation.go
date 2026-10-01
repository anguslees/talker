package orchestrator

import (
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/anguslees/talker/internal/tasks"
)

type conversationView struct {
	SessionID  string `json:"session_id"`
	StartedAt  string `json:"started_at"`
	ActiveTask string `json:"active_task_id,omitempty"`
}

type followView struct {
	SessionID    string `json:"session_id"`
	Title        string `json:"title,omitempty"`
	Conversation bool   `json:"current_conversation,omitempty"`
	Explicit     bool   `json:"requested_by_user,omitempty"`
	ActiveAt     string `json:"last_activity"`
}

func followViews(follows []tasks.Follow) []followView {
	out := make([]followView, 0, len(follows))
	for _, f := range follows {
		out = append(out, followView{SessionID: f.SessionID, Title: limitText(f.Title, 200), Conversation: f.Conversation, Explicit: f.Explicit, ActiveAt: f.ActiveAt.UTC().Format(time.RFC3339)})
	}
	return out
}

type followArgs struct {
	SessionID string `json:"session_id" jsonschema:"Exact Hermes session ID from hermes_sessions or list_tasks."`
}

type newConversationView struct {
	Ended string `json:"ended_session_id,omitempty"`
	Note  string `json:"note"`
}

type unfollowView struct {
	Unfollowed bool `json:"unfollowed"`
}

func conversationTools(manager *tasks.Manager) []built {
	return []built{
		wrap(functiontool.New(functiontool.Config{Name: "follow_session", Description: "Announce new Hermes replies in an existing session that the user continues elsewhere, such as the Hermes web UI or terminal, until they ask to stop. Only when the user asks. The current conversation is already followed."}, func(ctx agent.Context, a followArgs) (followView, error) {
			f, err := manager.Follow(ctx, a.SessionID)
			if err != nil {
				return followView{}, err
			}
			return followViews([]tasks.Follow{f})[0], nil
		})),
		wrap(functiontool.New(functiontool.Config{Name: "unfollow_session", Description: "Stop announcing replies from a followed Hermes session, when the user asks."}, func(ctx agent.Context, a followArgs) (unfollowView, error) {
			found, err := manager.Unfollow(a.SessionID)
			return unfollowView{Unfollowed: found}, err
		})),
		wrap(functiontool.New(functiontool.Config{Name: "new_conversation", Description: "Start a fresh Hermes conversation for the user's next requests, ONLY when the user asks for a new conversation or a clean slate. Running tasks are unaffected."}, func(ctx agent.Context, _ struct{}) (newConversationView, error) {
			ended, err := manager.NewConversation()
			if err != nil {
				return newConversationView{}, err
			}
			return newConversationView{Ended: ended, Note: "The next request starts a new Hermes conversation."}, nil
		})),
	}
}
