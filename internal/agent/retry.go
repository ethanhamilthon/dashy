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
)

const (
	retryAttempts = 3
	retryBase     = time.Second
	retryMax      = 8 * time.Second
)

func retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var api *apiError
	if errors.As(err, &api) {
		switch api.Status {
		case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			return true
		}
		return api.Status >= 500
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

func delay(attempt int, err error) time.Duration {
	wait := min(retryMax, retryBase<<attempt)
	var api *apiError
	if errors.As(err, &api) {
		if seconds, e := strconv.Atoi(api.RetryAfter); e == nil && seconds > 0 {
			seconds = min(seconds, int(retryMax/time.Second))
			wait = max(wait, time.Duration(seconds)*time.Second)
		}
	}
	return wait
}

func (a *Agent) requestWithRetries(ctx context.Context, emit func(Event)) ([]json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		shown := 0
		items, err := a.request(ctx, func(e Event) {
			if !e.Provisional || e.Kind == "delta" || e.Kind == "reasoning_delta" || e.Kind == "refusal_delta" {
				shown++
			}
			emit(e)
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
