package acp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"dashy/internal/agent"
)

type Options struct {
	Config   string
	NoGlobal bool
	Version  string
}
type session struct {
	a         *agent.Agent
	mu        sync.Mutex
	cancel    context.CancelFunc
	busy      bool
	cancelled bool
	closed    bool           // session/close removed it; no new work is admitted
	work      sync.WaitGroup // busy periods; close waits for them before releasing the agent
	models    map[string]agent.ModelChoice
}

// claim and release bracket a busy period; both run under session.mu.
func (session *session) claim()   { session.busy = true; session.work.Add(1) }
func (session *session) release() { session.busy = false; session.work.Done() }

type server struct {
	peer               *peer
	opts               Options
	mu                 sync.Mutex
	initialized        bool
	clientCapabilities json.RawMessage
	sessions           map[string]*session
	creating           int
}

// Run owns stdin/stdout until disconnect; neither stream carries human-oriented text.
func Run(ctx context.Context, in io.Reader, out io.Writer, opts Options) error {
	if file, ok := in.(*os.File); ok {
		stream, err := pollable(file)
		if err != nil {
			return err
		}
		defer stream.Close()
		in = stream
	}
	if file, ok := out.(*os.File); ok {
		stream, err := pollable(file)
		if err != nil {
			return err
		}
		defer stream.Close()
		out = stream
	}
	p := newPeer(ctx, in, out)
	s := &server{peer: p, opts: opts, sessions: map[string]*session{}}
	err := p.run(s.handle)
	for _, session := range s.sessions {
		session.a.Close()
	}
	return err
}
func decode(params json.RawMessage, v any) bool {
	return len(params) > 0 && string(params) != "null" && json.Unmarshal(params, v) == nil
}
func (s *server) job(fn func()) { s.peer.wg.Add(1); go func() { defer s.peer.wg.Done(); fn() }() }
func (s *server) handle(ctx context.Context, method string, params json.RawMessage, reply replyFunc) {
	if method == "initialize" {
		var p struct {
			ProtocolVersion    int             `json:"protocolVersion"`
			ClientCapabilities json.RawMessage `json:"clientCapabilities"`
		}
		if !decode(params, &p) || p.ProtocolVersion <= 0 {
			reply(nil, problem(-32602, "protocolVersion is required"))
			return
		}
		if s.initialized {
			reply(nil, problem(-32600, "Already initialized"))
			return
		}
		// Per v1, return the supported version; clients must disconnect on a mismatch.
		s.initialized = p.ProtocolVersion == 1
		s.clientCapabilities = p.ClientCapabilities
		reply(map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "dashy", "version": s.opts.Version}, "agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "close": map[string]any{}}, "promptCapabilities": map[string]bool{"image": false, "audio": false, "embeddedContext": false}, "mcpCapabilities": map[string]bool{"http": false, "sse": false}}, "authMethods": []any{}}, nil)
		return
	}
	if !s.initialized {
		reply(nil, problem(-32600, "Initialize ACP v1 first"))
		return
	}
	switch method {
	case "session/new", "session/load", "session/resume":
		s.openSession(ctx, params, reply, method)
	case "session/list":
		s.list(params, reply)
	case "session/close":
		s.closeSession(params, reply)
	case "session/prompt":
		s.prompt(ctx, params, reply)
	case "session/set_config_option":
		s.setConfig(ctx, params, reply)
	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if decode(params, &p) {
			if session := s.lookup(p.SessionID); session != nil {
				session.mu.Lock()
				if session.cancel != nil {
					session.cancelled = true
					session.cancel()
				}
				session.mu.Unlock()
			}
		}
		reply(nil, nil)
	case "authenticate":
		reply(nil, problem(-32602, "No interactive authentication; configure a provider in the Dashy TUI"))
	default:
		reply(nil, problem(-32601, "Method not found"))
	}
}
func (s *server) openSession(ctx context.Context, params json.RawMessage, reply replyFunc, method string) {
	load := method != "session/new"
	var p struct {
		SessionID             string            `json:"sessionId"`
		Cwd                   string            `json:"cwd"`
		MCPServers            []json.RawMessage `json:"mcpServers"`
		AdditionalDirectories []string          `json:"additionalDirectories"`
	}
	var required map[string]json.RawMessage
	valid := decode(params, &p) && decode(params, &required)
	if mcp := string(required["mcpServers"]); !valid || !filepath.IsAbs(p.Cwd) || (method != "session/resume" && (mcp == "" || mcp == "null")) {
		reply(nil, problem(-32602, "Absolute cwd and mcpServers array are required"))
		return
	}
	if load && (p.SessionID == "" || filepath.Base(p.SessionID) != p.SessionID || strings.ContainsAny(p.SessionID, `/\`)) {
		reply(nil, problem(-32602, "A valid sessionId is required"))
		return
	}
	if len(p.MCPServers) > 0 || len(p.AdditionalDirectories) > 0 {
		reply(nil, problem(-32602, "MCP servers and additional workspace roots are not supported by this preview"))
		return
	}
	if load {
		if existing := s.lookup(p.SessionID); existing != nil {
			existing.mu.Lock()
			if existing.busy || existing.closed || filepath.Clean(p.Cwd) != existing.a.Shell.Dir {
				existing.mu.Unlock()
				reply(nil, problem(-32600, "Session is busy or belongs to a different workspace"))
				return
			}
			existing.claim()
			existing.mu.Unlock()
			s.job(func() { s.sessionReady(ctx, existing, method, reply) })
			return
		}
	}
	s.mu.Lock()
	if len(s.sessions)+s.creating >= 16 {
		s.mu.Unlock()
		reply(nil, problem(-32603, "Session limit (16) exceeded"))
		return
	}
	s.creating++
	s.mu.Unlock()
	s.job(func() {
		defer func() { s.mu.Lock(); s.creating--; s.mu.Unlock() }()
		info, err := os.Stat(p.Cwd)
		if err != nil || !info.IsDir() {
			reply(nil, problem(-32602, "cwd must be an existing local directory"))
			return
		}
		session := &session{}
		opts := agent.Options{Mode: "acp", Config: s.opts.Config, NoGlobal: s.opts.NoGlobal}
		if load {
			opts.Resume = p.SessionID
		}
		opts.Observe = func(e agent.ExecutionEvent) { s.tool(session, e) }
		a, err := agent.New(p.Cwd, opts)
		if err != nil {
			reply(nil, problem(-32603, "Unable to open local session storage or instructions"))
			return
		}
		session.a = a
		if ctx.Err() != nil {
			a.Close()
			reply(nil, problem(-32800, "Request cancelled"))
			return
		}
		if a.Settings().SetupHint() != "" {
			a.Close()
			reply(nil, problem(-32000, "Configure a provider and model using the Dashy TUI before connecting"))
			return
		}
		session.discoverModels(ctx)
		if ctx.Err() != nil {
			a.Close()
			reply(nil, problem(-32800, "Request cancelled"))
			return
		}
		session.claim()
		s.mu.Lock()
		s.sessions[a.SessionID()] = session
		s.mu.Unlock()
		s.sessionReady(ctx, session, method, reply)
	})
}
func (s *server) lookup(id string) *session { s.mu.Lock(); defer s.mu.Unlock(); return s.sessions[id] }
func (s *server) prompt(ctx context.Context, params json.RawMessage, reply replyFunc) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string
			Text *string
		} `json:"prompt"`
	}
	if !decode(params, &p) || p.SessionID == "" || len(p.Prompt) == 0 {
		reply(nil, problem(-32602, "sessionId and a nonempty prompt are required"))
		return
	}
	var text []string
	for _, block := range p.Prompt {
		if block.Type != "text" || block.Text == nil {
			reply(nil, problem(-32602, "Only text prompt blocks with text are supported"))
			return
		}
		text = append(text, *block.Text)
	}
	input := strings.Join(text, "\n")
	if strings.TrimSpace(input) == "" {
		reply(nil, problem(-32602, "Prompt must contain text"))
		return
	}
	session := s.lookup(p.SessionID)
	if session == nil {
		reply(nil, problem(-32002, "Unknown session"))
		return
	}
	session.mu.Lock()
	if session.busy || session.closed {
		session.mu.Unlock()
		reply(nil, problem(-32600, "A prompt is already running in this session"))
		return
	}
	turn, cancel := context.WithCancel(ctx)
	session.claim()
	session.cancelled = false
	session.cancel = cancel
	session.mu.Unlock()
	s.job(func() {
		defer cancel()
		// Only committed text is published: retry rollbacks never duplicate editor output.
		err := session.a.Run(turn, input, func(e agent.Event) {
			if e.Kind == "assistant" && e.Final && !e.Provisional {
				s.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": agent.Redact(session.a.Settings(), e.Text)}})
			}
		})
		s.usage(session)
		session.mu.Lock()
		stopped, sessionCancelled := turn.Err() != nil, session.cancelled
		session.release()
		session.cancel = nil
		session.mu.Unlock()
		if sessionCancelled {
			reply(map[string]string{"stopReason": "cancelled"}, nil)
		} else if stopped {
			reply(nil, problem(-32800, "Request cancelled"))
		} else if err != nil {
			code := -32603
			if agent.AuthenticationRequired(err) {
				code = -32000
			}
			reply(nil, problem(code, agent.Redact(session.a.Settings(), err.Error())))
		} else {
			reply(map[string]string{"stopReason": "end_turn"}, nil)
		}
	})
}
func (s *server) update(session *session, update any) {
	_ = s.peer.notify(map[string]any{"sessionId": session.a.SessionID(), "update": update})
}
func (s *server) tool(session *session, e agent.ExecutionEvent) {
	clean := func(text string) string { return agent.Redact(session.a.Settings(), text) }
	update := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": e.ID, "status": e.Status}
	if e.Status == "pending" {
		update["sessionUpdate"] = "tool_call"
		update["title"] = clean(e.Command)
		update["kind"] = "execute"
		update["rawInput"] = map[string]string{"command": clean(e.Command)}
	}
	if e.Status == "completed" || e.Status == "failed" {
		output := clean(e.Result.Output)
		if len(output) > 64<<10 {
			output = output[:64<<10] + "\n[ACP output truncated]"
		}
		update["content"] = []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": output}}}
		update["rawOutput"] = map[string]any{"exitCode": e.Result.ExitCode, "error": e.Result.Error, "durationMs": e.Duration.Milliseconds()}
	}
	s.update(session, update)
}
