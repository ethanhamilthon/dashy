package agent

import (
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
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
	var api *openai.Error
	if errors.As(err, &api) {
		text = api.Error() // status line only; the message is the one provider field kept
		if api.Message != "" {
			text += ": " + api.Message
		}
	}
	return &providerError{text: Redact(a.settings, text), cause: err}
}

// streamFailure reports a failed stream event by its code and message, never the raw event.
func streamFailure(event responses.ResponseStreamEventUnion) error {
	fields := []string{event.Code, event.Message}
	switch event.Type {
	case "response.failed":
		fields = []string{string(event.Response.Error.Code), event.Response.Error.Message}
	case "response.incomplete":
		fields = []string{event.Response.IncompleteDetails.Reason}
	}
	return fmt.Errorf("provider stream %s: %s", event.Type, strings.TrimSpace(strings.Join(fields, " ")))
}
