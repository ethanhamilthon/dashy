package agent

import (
	"dashy/internal/shell"
	"time"
)

// Execution describes arbitrary local code, not a read-only/write-only classification.
type Execution struct {
	ID, Kind, Command string
	Started           time.Time
}
type ExecutionEvent struct {
	Execution
	Status   string
	Result   shell.Result
	Duration time.Duration
}

func (a *Agent) observe(e Execution, status string, result shell.Result) {
	if a.opts.Observe != nil {
		a.opts.Observe(ExecutionEvent{e, status, result, time.Since(e.Started)})
	}
}
func (a *Agent) completed(e Execution, result shell.Result) {
	status := "completed"
	if result.Error {
		status = "failed"
	}
	a.observe(e, status, result)
}
