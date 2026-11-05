package agent

import (
	"fmt"
	"strings"
)

// providerError holds sanitized text; Unwrap keeps retry and cancellation checks working.
type providerError struct {
	text  string
	cause error
}

func (e *providerError) Error() string { return e.text }
func (e *providerError) Unwrap() error { return e.cause }

// safeError is the form of err that events, records, and callers see: configured
// keys removed, and an HTTP failure reduced to its status and message.
func (a *Agent) safeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*providerError); ok {
		return err
	}
	text := err.Error()
	return &providerError{text: Redact(a.Settings(), text), cause: err}
}

// streamFailure reports a failed stream event by its code and message, never the raw event.
func streamFailure(event streamEvent) error {
	fields := []string{event.Code, event.Message}
	switch {
	case event.Type == "response.failed" && event.Response.Error != nil:
		fields = []string{event.Response.Error.Code, event.Response.Error.Message}
	case event.Type == "response.incomplete" && event.Response.IncompleteDetails != nil:
		fields = []string{event.Response.IncompleteDetails.Reason}
	}
	return fmt.Errorf("provider stream %s: %s", event.Type, strings.TrimSpace(strings.Join(fields, " ")))
}
