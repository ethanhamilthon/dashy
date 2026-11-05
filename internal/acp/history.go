package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dashy/internal/agent"
	"dashy/internal/shell"
)

func (s *server) sessionReady(ctx context.Context, session *session, method string, reply replyFunc) {
	// Hold the session busy until replay finishes; prompts must not interleave with it.
	// session/resume restores the same state but leaves the transcript to the client.
	var result any
	var failure *rpcError
	defer func() {
		session.mu.Lock()
		defer session.mu.Unlock()
		session.release()
		reply(result, failure)
	}()
	if method == "session/load" {
		if err := s.replay(ctx, session); err != nil {
			failure = problem(-32800, "History replay cancelled")
			return
		}
	}
	if err := s.usageWait(ctx, session); err != nil {
		failure = problem(-32800, "Request cancelled")
		return
	}
	response := map[string]any{"configOptions": session.configOptions()}
	if method == "session/new" {
		response["sessionId"] = session.a.SessionID()
	}
	result = response
}

func (s *server) replay(ctx context.Context, session *session) error {
	publish := func(update any) error {
		return s.peer.sendWait(ctx, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session.a.SessionID(), "update": update}})
	}
	clean := func(text string) string { return agent.Redact(session.a.Settings(), text) }
	calls := map[string]string{}
	for seq, raw := range session.a.History {
		if err := ctx.Err(); err != nil {
			return err
		}
		var item struct {
			Type, Name, Arguments, Output string
			CallID                        string `json:"call_id"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		switch item.Type {
		case "function_call":
			id := fmt.Sprintf("history:%d:%s", seq, item.CallID)
			calls[item.CallID] = id
			var args struct{ Command string }
			_ = json.Unmarshal([]byte(item.Arguments), &args)
			title, kind := item.Name, "other"
			if item.Name == "bash" {
				title, kind = args.Command, "execute"
			}
			if err := publish(map[string]any{"sessionUpdate": "tool_call", "toolCallId": id, "title": clean(title), "kind": kind, "status": "pending", "rawInput": clean(item.Arguments)}); err != nil {
				return err
			}
		case "function_call_output":
			id, ok := calls[item.CallID]
			if !ok {
				continue
			}
			var result shell.Result
			if json.Unmarshal([]byte(item.Output), &result) != nil {
				result.Output = item.Output
			}
			status := "completed"
			if result.Error {
				status = "failed"
			}
			output := clean(result.Output)
			if len(output) > 64<<10 {
				output = output[:64<<10] + "\n[ACP output truncated]"
			}
			if err := publish(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status, "content": []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": output}}}, "rawOutput": map[string]any{"exitCode": result.ExitCode, "error": result.Error}}); err != nil {
				return err
			}
		default:
			for _, record := range agent.RecordsForItem(raw) {
				var kind string
				switch record.Kind {
				case "user":
					kind = "user_message_chunk"
					// Attachment originals are model context only; the images follow as blocks.
					record.Text, _, _ = strings.Cut(record.Text, "\nOriginal image (for crops): ")
					if err := s.replayImages(raw, publish); err != nil {
						return err
					}
				case "assistant":
					kind = "agent_message_chunk"
				case "reasoning":
					kind = "agent_thought_chunk"
				default:
					continue
				}
				if err := publish(map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": clean(record.Text)}}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// replayImages publishes the images of a user history item as ACP image blocks.
func (s *server) replayImages(raw json.RawMessage, publish func(any) error) error {
	var message struct {
		Content []struct {
			Type     string `json:"type"`
			ImageURL string `json:"image_url"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &message) // plain text content has no images
	for _, part := range message.Content {
		mime, data, ok := strings.Cut(strings.TrimPrefix(part.ImageURL, "data:"), ";base64,")
		if part.Type != "input_image" || !ok {
			continue
		}
		if err := publish(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "image", "mimeType": mime, "data": data}}); err != nil {
			return err
		}
	}
	return nil
}

func (session *session) usageUpdate() map[string]any {
	settings := session.a.Settings()
	size := session.models[modelID(settings.Provider, settings.Model)].ContextWindow
	// Providers may omit model limits. Use a display-only fallback, never a request limit.
	if size <= 0 {
		size = 1_000_000
	}
	return map[string]any{"sessionUpdate": "usage_update", "used": max(session.a.ContextTokens(), 0), "size": size}
}

func (s *server) usageWait(ctx context.Context, session *session) error {
	if update := session.usageUpdate(); update != nil {
		return s.peer.sendWait(ctx, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session.a.SessionID(), "update": update}})
	}
	return ctx.Err()
}

func (s *server) usage(session *session) {
	if update := session.usageUpdate(); update != nil {
		s.update(session, update)
	}
}
