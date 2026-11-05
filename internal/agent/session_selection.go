package agent

import (
	"context"
	"errors"
)

// SelectSession changes only this session, without changing shared TUI defaults.
// Call between turns. Failed validation or persistence restores the old selection.
func (a *Agent) SelectSession(ctx context.Context, provider, model, effort string) error {
	if provider == "" || model == "" {
		return errors.New("provider and model are required")
	}
	c := a.cfg
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.sel
	c.sel = selection{provider, model, effort}
	err := a.applyLocked(c.base, c.version)
	if err == nil {
		_, err = a.db.ExecContext(ctx, `UPDATE sessions SET provider = ?, model = ?, effort = ? WHERE id = ?`, provider, model, effort, a.id)
	}
	if err != nil {
		c.sel = previous
		_ = a.applyLocked(c.base, c.version)
	}
	return err
}
