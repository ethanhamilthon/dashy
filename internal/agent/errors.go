package agent

import (
	"errors"
	"fmt"
	"strings"
)

type providerError struct {
	text  string
	cause error
}

func (e *providerError) Error() string { return e.text }
func (e *providerError) Unwrap() error { return e.cause }

func AuthenticationRequired(err error) bool {
	var api *apiError
	return errors.As(err, &api) && (api.Status == 401 || api.Status == 403)
}

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

func streamFailure(event streamEvent) error {
	fields := []string{event.Code, event.Message}
	switch {
	case event.Error != nil:
		fields = []string{event.Error.Code, event.Error.Message}
	case event.Type == "response.failed" && event.Response.Error != nil:
		fields = []string{event.Response.Error.Code, event.Response.Error.Message}
	case event.Type == "response.incomplete" && event.Response.IncompleteDetails != nil:
		fields = []string{event.Response.IncompleteDetails.Reason}
	}
	detail := strings.TrimSpace(strings.Join(fields, " "))
	if detail == "" {
		detail = "provider reported a failure without details"
	}
	return fmt.Errorf("provider stream %s: %s", event.Type, detail)
}
