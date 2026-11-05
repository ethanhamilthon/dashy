package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type client struct {
	base, key string
}

var httpClient = &http.Client{Timeout: 10 * time.Minute}

func newClient(p Provider) *client {
	return &client{base: strings.TrimRight(p.BaseURL, "/"), key: p.key()}
}

type apiError struct {
	Status     int
	Message    string
	RetryAfter string
}

func (e *apiError) Error() string {
	text := fmt.Sprintf("provider returned %d %s", e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

func (c *client) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Error json.RawMessage
		}
		_ = json.Unmarshal(data, &e)
		var message struct{ Message string }
		if json.Unmarshal(e.Error, &message) != nil {
			_ = json.Unmarshal(e.Error, &message.Message) // some providers send a plain string
		}
		return nil, &apiError{Status: resp.StatusCode, Message: message.Message, RetryAfter: resp.Header.Get("Retry-After")}
	}
	return resp, nil
}

// models lists the model IDs of the provider, following pagination. A page that
// brings no new model ends the listing, so a looping provider cannot hang it.
func (c *client) models(ctx context.Context) ([]string, error) {
	models, err := c.modelCatalog(ctx)
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids, err
}

type modelInfo struct {
	ID            string
	ContextLength int64 `json:"context_length"`
	ContextWindow int64 `json:"context_window"`
}

func (m modelInfo) window() int64 {
	if m.ContextLength > 0 {
		return m.ContextLength
	}
	return max(m.ContextWindow, 0)
}

func (c *client) modelCatalog(ctx context.Context) ([]modelInfo, error) {
	var ids []modelInfo
	seen := map[string]bool{}
	for after, pages := "", 0; pages < maxModelPages; pages++ {
		path := "/models"
		if after != "" {
			path += "?after=" + url.QueryEscape(after)
		}
		resp, err := c.send(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data    []modelInfo
			HasMore bool `json:"has_more"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("models: %w", err)
		}
		fresh := 0
		for _, m := range page.Data {
			if !seen[m.ID] {
				seen[m.ID] = true
				ids = append(ids, m)
				fresh++
			}
		}
		if !page.HasMore || fresh == 0 {
			return ids, nil
		}
		after = page.Data[len(page.Data)-1].ID
	}
	return ids, nil
}

const maxModelPages = 100

type streamEvent struct {
	Type        string                          `json:"type"`
	OutputIndex int64                           `json:"output_index"`
	Delta       string                          `json:"delta"`
	Arguments   string                          `json:"arguments"`
	Item        json.RawMessage                 `json:"item"`
	Code        string                          `json:"code"`
	Message     string                          `json:"message"`
	Error       *struct{ Code, Message string } `json:"error"`
	Response    struct {
		Output []json.RawMessage `json:"output"`
		Usage  *struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

// errStreamDone, returned by a stream callback, ends the stream successfully: a
// provider may keep the connection open after its terminal event.
var errStreamDone = errors.New("stream done")

const (
	maxStreamEvent  = 8 << 20
	maxStreamBytes  = 64 << 20
	maxStreamEvents = 100000
)

func (c *client) stream(ctx context.Context, body map[string]any, fn func(streamEvent) error) error {
	body["stream"] = true
	resp, err := c.send(ctx, http.MethodPost, "/responses", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	budget := &io.LimitedReader{R: resp.Body, N: maxStreamBytes + 1}
	scanner := bufio.NewScanner(budget)
	scanner.Buffer(make([]byte, 64<<10), maxStreamEvent)
	var data strings.Builder
	events := 0
	for scanner.Scan() {
		if budget.N == 0 {
			return errors.New("provider stream: response byte limit exceeded")
		}
		line := scanner.Text()
		if line != "" {
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				if data.Len()+len(rest) > maxStreamEvent {
					return errors.New("provider stream: event byte limit exceeded")
				}
				data.WriteString(strings.TrimPrefix(rest, " "))
			}
			continue
		}
		// Count empty events too so comment/heartbeat floods have a budget.
		events++
		if events > maxStreamEvents {
			return errors.New("provider stream: event count limit exceeded")
		}
		if data.Len() == 0 || data.String() == "[DONE]" {
			data.Reset()
			continue
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
			return fmt.Errorf("provider stream: invalid event: %w", err)
		}
		data.Reset()
		if err := fn(event); errors.Is(err, errStreamDone) {
			return nil
		} else if err != nil {
			return err
		}
	}
	if budget.N == 0 {
		return errors.New("provider stream: response byte limit exceeded")
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("provider stream: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if data.Len() > 0 && data.String() != "[DONE]" {
		var event streamEvent
		if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
			return fmt.Errorf("provider stream: invalid event: %w", err)
		}
		return fn(event)
	}
	return nil
}
