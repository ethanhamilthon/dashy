// Package acp implements the pinned ACP v1 local stdio preview (see docs/ACP.md).
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"
)

const (
	maxFrame     = 8 << 20
	maxPending   = 64
	writeTimeout = 5 * time.Second
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func problem(code int, text string) *rpcError { return &rpcError{code, text} }

type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type replyFunc func(any, *rpcError)
type peer struct {
	ctx    context.Context
	cancel context.CancelFunc
	in     io.Reader
	out    io.Writer
	queue  chan []byte
	mu     sync.Mutex
	active map[string]context.CancelFunc
	wg     sync.WaitGroup
	err    error
	once   sync.Once
}

func newPeer(ctx context.Context, in io.Reader, out io.Writer) *peer {
	ctx, cancel := context.WithCancel(ctx)
	return &peer{ctx: ctx, cancel: cancel, in: in, out: out, queue: make(chan []byte, maxPending), active: map[string]context.CancelFunc{}}
}
func (p *peer) stop(err error) {
	p.once.Do(func() {
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		p.cancel()
		if c, ok := p.in.(io.Closer); ok {
			_ = c.Close()
		}
		if c, ok := p.out.(io.Closer); ok {
			_ = c.Close()
		}
	})
}
func (p *peer) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil || len(b) > maxFrame {
		p.stop(errors.New("ACP output frame limit"))
		return errors.New("ACP output frame limit")
	}
	select {
	case <-p.ctx.Done():
		return p.ctx.Err()
	default:
	}
	select {
	case p.queue <- append(b, '\n'):
		return nil
	default:
		p.stop(errors.New("ACP output queue full"))
		return errors.New("ACP output queue full")
	}
}

// sendWait applies backpressure to bulk history replay without dropping frames.
// The writer's timeout and connection cancellation bound a stalled consumer.
func (p *peer) sendWait(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil || len(b) > maxFrame {
		p.stop(errors.New("ACP output frame limit"))
		return errors.New("ACP output frame limit")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	case p.queue <- append(b, '\n'):
		return nil
	}
}

func (p *peer) notify(params any) error {
	return p.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": params})
}
func (p *peer) response(id json.RawMessage, value any, e *rpcError) {
	m := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		m["error"] = e
	} else {
		m["result"] = value
	}
	_ = p.sendWait(p.ctx, m)
}
func (p *peer) writeLoop() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case data := <-p.queue:
			timer := time.AfterFunc(writeTimeout, func() { p.stop(errors.New("ACP writer stalled")) })
			n, err := p.out.Write(data)
			timer.Stop()
			if err != nil || n != len(data) {
				p.stop(errors.New("ACP write failed"))
				return
			}
		}
	}
}
func idKey(id json.RawMessage) (string, bool) {
	if len(id) == 0 {
		return "", false
	}
	d := json.NewDecoder(bytes.NewReader(id))
	d.UseNumber()
	var v any
	if d.Decode(&v) == nil {
		switch value := v.(type) {
		case string:
			return "s:" + value, true
		case json.Number:
			if n, err := value.Int64(); err == nil {
				return "n:" + strconv.FormatInt(n, 10), true
			}
		}
	}
	return "", false
}
func (p *peer) run(handle func(context.Context, string, json.RawMessage, replyFunc)) error {
	go p.writeLoop()
	go func() { <-p.ctx.Done(); p.stop(p.ctx.Err()) }()
	scan := bufio.NewScanner(p.in)
	scan.Buffer(make([]byte, 64<<10), maxFrame)
	for scan.Scan() {
		line := bytes.TrimSpace(scan.Bytes())
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			p.response(json.RawMessage("null"), nil, problem(-32700, "Parse error"))
			continue
		}
		var f frame
		if line[0] != '{' || json.Unmarshal(line, &f) != nil || f.JSONRPC != "2.0" {
			p.response(json.RawMessage("null"), nil, problem(-32600, "Invalid request"))
			continue
		}
		key, validID := idKey(f.ID)
		if f.Method == "" {
			if !validID || (len(f.Result) == 0) == (f.Error == nil) {
				p.response(json.RawMessage("null"), nil, problem(-32600, "Invalid response"))
				continue
			}

			continue
		}
		if len(f.Result) > 0 || f.Error != nil || (len(f.ID) > 0 && !validID) {
			p.response(json.RawMessage("null"), nil, problem(-32600, "Invalid request"))
			continue
		}
		if len(f.Params) > 0 && f.Params[0] != '{' {
			if validID {
				p.response(f.ID, nil, problem(-32602, "Invalid params"))
			}
			continue
		}
		if !validID {
			if f.Method == "$/cancel_request" {
				var v struct {
					ID json.RawMessage `json:"id"`
				}
				_ = json.Unmarshal(f.Params, &v)
				k, _ := idKey(v.ID)
				p.mu.Lock()
				cancel := p.active[k]
				p.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			} else if f.Method == "session/cancel" {
				handle(p.ctx, f.Method, f.Params, func(any, *rpcError) {})
			}
			continue
		}
		ctx, cancel := context.WithCancel(p.ctx)
		p.mu.Lock()
		_, duplicate := p.active[key]
		limited := len(p.active) >= maxPending
		if !duplicate && !limited {
			p.active[key] = cancel
		}
		p.mu.Unlock()
		if duplicate {
			cancel()
			p.stop(errors.New("ACP duplicate in-flight request ID"))
			break
		}
		if limited {
			cancel()
			p.response(f.ID, nil, problem(-32603, "Request limit exceeded"))
			continue
		}
		var once sync.Once
		id := append(json.RawMessage(nil), f.ID...)
		respond := func(v any, e *rpcError) {
			once.Do(func() { p.response(id, v, e); p.mu.Lock(); delete(p.active, key); p.mu.Unlock(); cancel() })
		}
		handle(ctx, f.Method, f.Params, respond)
	}
	err := scan.Err()
	if err != nil {
		err = errors.New("ACP read failed or frame exceeds 8 MiB")
	}
	p.stop(err)
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
