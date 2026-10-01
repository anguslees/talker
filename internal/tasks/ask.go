package tasks

import (
	"context"
	"time"
)

// Ask submits like Start, then waits up to wait for the run to finish or to
// need approval. A finished run's outcome goes to the caller instead of the
// notification queue; an approval request is queued as usual. A run still
// working after wait continues as an ordinary task and is announced when it
// finishes. The error reports a tracked task's uncertain admission; an
// untracked request returns a zero Task.
func (m *Manager) Ask(ctx context.Context, prompt, sessionID string, wait time.Duration) (Task, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	task, admitErr := m.start(ctx, prompt, sessionID, true)
	if _, tracked := m.Get(task.ID); !tracked {
		return Task{}, false, admitErr
	}
	if ctx.Err() != nil {
		// The wait ended during admission; the tracked run continues.
		admitErr = nil
	}
	for {
		m.mu.Lock()
		rec := m.state.Tasks[task.ID]
		settled := finished(rec.Task.Status) || rec.Task.Status == "waiting_for_approval"
		// Settlement and releasing the inline claim must be one step: a run that
		// finishes after the claim is released is announced instead.
		if settled || ctx.Err() != nil || m.isClosedLocked() {
			delete(m.inline, task.ID)
			m.mu.Unlock()
			return cloneTask(rec.Task), settled, admitErr
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		case <-m.ctx.Done():
		}
	}
}
