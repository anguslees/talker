package tasks

import (
	"encoding/json"

	"talker/internal/hermes"
)

// Recent returns bounded previews; Get retains the complete persisted result.
func (m *Manager) Recent(limit int) []Task {
	list := m.List()
	result := make([]Task, 0)
	budget := 512 << 10
	for _, task := range list {
		if len(result) >= limit {
			break
		}
		task.Prompt = preview(task.Prompt, 2000)
		task.Output = preview(task.Output, 4000)
		task.Error = preview(task.Error, 1000)
		task.PendingSteer = nil
		if task.Approval != nil {
			task.Approval.Command = preview(task.Approval.Command, 4000)
			task.Approval.Description = preview(task.Approval.Description, 2000)
		}
		encoded, _ := json.Marshal(task)
		if len(encoded) > budget {
			break
		}
		budget -= len(encoded) + 1
		result = append(result, task)
	}
	return result
}

// Pending pages oldest unread notifications without changing their delivery state.
func (m *Manager) Pending(limit int) []Event {
	result := make([]Event, 0)
	budget := 512 << 10
	for _, event := range m.Events() {
		if len(result) >= limit {
			break
		}
		event.Body = preview(event.Body, 4000)
		encoded, _ := json.Marshal(event)
		if len(encoded) > budget {
			break
		}
		budget -= len(encoded) + 1
		result = append(result, event)
	}
	return result
}

func preview(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + " [truncated; use get_task for task details]"
}

// Client exposes the Hermes connection for read-only catalogue and session
// queries that do not belong in the task ledger.
func (m *Manager) Client() *hermes.Client { return m.client }
