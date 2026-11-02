package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
)

const (
	retryAttempts = 3
	retryBase     = time.Second
	retryMax      = 8 * time.Second
)

// retryable reports whether err deserves another attempt before any output was shown.
func retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var api *openai.Error
	if errors.As(err, &api) {
		switch api.StatusCode {
		case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			return true
		}
		return api.StatusCode >= 500
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	for _, text := range []string{"connection reset", "connection refused", "unexpected EOF", "provider stream ended"} {
		if strings.Contains(err.Error(), text) {
			return true
		}
	}
	return errors.Is(err, io.EOF)
}

// delay returns the pause before the next attempt. A provider Retry-After wins when longer.
func delay(attempt int, err error) time.Duration {
	wait := min(retryMax, retryBase<<attempt)
	var api *openai.Error
	if errors.As(err, &api) && api.Response != nil {
		if seconds, e := strconv.Atoi(api.Response.Header.Get("Retry-After")); e == nil && seconds > 0 {
			wait = max(wait, time.Duration(seconds)*time.Second)
		}
	}
	return wait
}

// requestWithRetries repeats a failed request while nothing has been shown to the user yet.
// Retrying after output would duplicate text, so that case returns the error unchanged.
func (a *Agent) requestWithRetries(ctx context.Context, emit func(Event), checkpoint func(int64, json.RawMessage) error) ([]json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		shown := 0
		items, err := a.request(ctx, func(e Event) {
			if !e.Provisional || e.Kind == "delta" || e.Kind == "reasoning_delta" || e.Kind == "refusal_delta" {
				shown++
			}
			emit(e)
		}, func(index int64, item json.RawMessage) error {
			shown++ // A completed item must never be replayed by a retry.
			return checkpoint(index, item)
		})
		if err == nil {
			return items, nil
		}
		if shown > 0 || attempt == retryAttempts-1 || !retryable(err) {
			return nil, err
		}
		pause := delay(attempt, err)
		emit(Event{Kind: "notice", Text: fmt.Sprintf("Retrying in %s (attempt %d/%d): %s", pause.Round(time.Second), attempt+2, retryAttempts, err)})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pause):
		}
	}
}
