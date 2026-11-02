package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dashy/internal/agent"
	"dashy/internal/control"
	"dashy/internal/shell"
	"dashy/internal/tui"
)

// The JSON transport is newline-delimited UTF-8 JSON in both directions. Every
// request carries an id and receives exactly one result. Controller events
// follow in emission order. Stdout carries only protocol lines; diagnostics go
// to stderr.
const (
	protocolVersion   = 1
	maxRecordBytes    = 2 * 1024 * 1024
	outboxLimit       = 4096
	maxAsyncRequests  = 8
	maxImagesPerInput = 8
	shutdownGrace     = 10 * time.Second
	flushGrace        = 2 * time.Second // how long a closing transport waits for stdout to drain
)

// request is one stdin record. Images and files are paths resolved by the transport.
type request struct {
	ID     string            `json:"id"`
	Type   string            `json:"type"`
	Text   string            `json:"text"`
	Name   string            `json:"name"`
	Images []string          `json:"images"`
	Files  []string          `json:"files"`
	Args   map[string]string `json:"args"`
}

// serveJSON runs the transport until stdin ends, the controller closes, or the
// process is interrupted. Only the controller changes application state; this
// function frames records, writes results and events, and tracks listings.
func serveJSON(ctx context.Context, a *agent.Agent, cwd string, in io.Reader, out, diagnostics io.Writer) error {
	t := newTransport(out)
	c := control.New(a, func(e control.Event) { t.emit(e) })
	c.SetInteractive(false)
	runCtx, stopRun := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = c.Run(runCtx)
	}()

	// Model listings run under listCtx. Shutdown cancels them and waits for their
	// results to be written, so the final record always comes last.
	listCtx, cancelListings := context.WithCancel(ctx)
	pending := newInFlight()
	reader := startReader(in)
	records := reader.records
	var drained <-chan struct{} // closed once stdin has ended and accepted work has settled
	defer func() {
		stopListings(cancelListings, pending)
		stopRun()
		joinWithin(runDone, shutdownGrace)
		if drained != nil {
			joinWithin(drained, shutdownGrace)
		}
		reader.closeReader(shutdownGrace)
		t.close()
	}()

	t.emit(readyRecord(c, cwd))
	for {
		select {
		case <-ctx.Done():
			stopListings(cancelListings, pending)
			interrupt(c)
			t.finish("signal")
			return ctx.Err()
		case <-t.broken:
			stopListings(cancelListings, pending)
			interrupt(c)
			t.finish("broken_output")
			return fmt.Errorf("writing protocol output: %w", t.failure())
		case <-t.overflow:
			stopListings(cancelListings, pending)
			interrupt(c)
			t.finish("output_overflow")
			fmt.Fprintln(diagnostics, "dashy: JSON output backlog exceeded; the session was stopped")
			return errors.New("JSON output backlog exceeded")
		case <-c.Closed():
			stopListings(cancelListings, pending)
			t.finish(c.Reason())
			return nil
		case <-drained:
			// Accepted work and listings have finished (or the grace period ended): save and exit.
			err := c.Shutdown("eof")
			t.finish("eof")
			if err == nil && t.failure() != nil {
				err = fmt.Errorf("writing protocol output: %w", t.failure())
			}
			return err
		case rec, ok := <-records:
			if !ok {
				// Stdin ended: stop reading and let accepted work drain. The other
				// cases stay live, so a broken stdout still cancels that work.
				records = nil
				drained = drain(c, pending, cancelListings)
				continue
			}
			handleRecord(listCtx, c, cwd, rec, t, pending)
		}
	}
}

// drain runs after stdin ends. It waits for accepted operations and listings;
// one shutdownGrace deadline covers both. When it passes, the operation is
// interrupted and listings are cancelled, and their results are still written.
func drain(c *control.Controller, pending *inFlight, cancelListings context.CancelFunc) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := c.Wait(ctx); err != nil {
			interrupt(c)
		}
		if err := pending.wait(ctx); err != nil {
			stopListings(cancelListings, pending)
		}
	}()
	return done
}

// stopListings cancels accepted listings and waits, for at most shutdownGrace,
// until each has written its result. Call it before the transport closes.
func stopListings(cancel context.CancelFunc, pending *inFlight) {
	cancel()
	ctx, done := context.WithTimeout(context.Background(), shutdownGrace)
	defer done()
	_ = pending.wait(ctx)
}

// interrupt stops active work so its turn ends with a cancelled done event.
func interrupt(c *control.Controller) {
	_, _ = c.Submit(control.Request{Type: "stop"})
	waitCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = c.Wait(waitCtx)
}

// handleRecord decodes one stdin record and answers it. A slash command is
// normalized before anything else, so it is classified by the operation it
// names. Model listings run in the background so stop and steering stay
// responsive while they wait.
func handleRecord(listCtx context.Context, c *control.Controller, cwd string, rec inputRecord, t *transport, pending *inFlight) {
	if rec.tooLong {
		t.protocolError("record_too_large", fmt.Sprintf("input record exceeds %d bytes", maxRecordBytes))
		return
	}
	var req request
	if err := json.Unmarshal(rec.line, &req); err != nil {
		t.protocolError("invalid_json", "input record is not a valid JSON object")
		return
	}
	if req.ID == "" {
		t.protocolError("missing_id", "every request needs a non-empty id")
		return
	}
	mapped, normErr := control.Normalize(control.Request{ID: req.ID, Type: req.Type, Text: req.Text, Name: req.Name, Args: req.Args})
	listing := normErr == nil && mapped.Type == "models"
	if !pending.begin(req.ID, listing) {
		busy := &control.Error{Code: "busy", Message: "too many model listings are in flight"}
		code := &control.Error{Code: "duplicate_id", Message: "a request with this id is still in flight"}
		if pending.has(req.ID) {
			t.result(req.ID, control.Response{}, code)
		} else {
			t.result(req.ID, control.Response{}, busy)
		}
		return
	}
	if normErr != nil {
		t.result(req.ID, control.Response{}, normErr)
		pending.end(req.ID, false)
		return
	}
	if listing {
		go func() {
			defer pending.end(req.ID, true)
			resp, err := c.SubmitContext(listCtx, mapped)
			t.result(req.ID, resp, err)
		}()
		return
	}
	defer pending.end(req.ID, false)
	prepared, err := prepare(c, cwd, mapped, req.Images, req.Files)
	if err != nil {
		t.result(req.ID, control.Response{}, err)
		return
	}
	resp, err := c.Submit(prepared)
	if err != nil {
		discardSnapshots(prepared.Images)
	}
	t.result(req.ID, resp, err)
}

// prepare converts paths into image and file snapshots before the controller sees the request.
// Only input and steer carry attachments, so other requests skip this work. Copies made
// before a failure are removed here; a rejected Submit removes them in handleRecord.
func prepare(c *control.Controller, cwd string, req control.Request, imagePaths, filePaths []string) (control.Request, error) {
	out := req
	out.Images, out.Files = nil, nil
	if req.Type != "input" && req.Type != "steer" {
		return out, nil
	}
	settings := c.Settings()
	if len(imagePaths) > maxImagesPerInput {
		return out, &control.Error{Code: "attachment_limit", Message: fmt.Sprintf("at most %d images per request", maxImagesPerInput)}
	}
	for _, path := range imagePaths {
		image, err := shell.CaptureImageCopy(resolvePath(cwd, path), attachmentDirectory(settings, cwd))
		if err != nil {
			discardSnapshots(out.Images)
			return out, &control.Error{Code: "invalid_image", Message: fmt.Sprintf("%s: %v", path, err)}
		}
		out.Images = append(out.Images, image)
	}
	if len(filePaths) > 0 {
		files, err := agent.ReadTextFiles(cwd, filePaths, settings.MaxFileBytes)
		if err != nil {
			discardSnapshots(out.Images)
			return out, &control.Error{Code: "invalid_file", Message: err.Error()}
		}
		out.Files = files
	}
	return out, nil
}

// discardSnapshots removes copies of a request that did not start. Paths come only from
// CaptureImageCopy, which always creates a new file, so the user's originals are never removed.
func discardSnapshots(images []shell.Image) {
	for _, image := range images {
		_ = os.Remove(image.Path)
	}
}

// resolvePath resolves relative paths against the project directory.
func resolvePath(cwd, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(cwd, path)
}

// attachmentDirectory returns attachment_dir when set, else $DASHY_HOME/attachments (dashy view always uses the latter).
func attachmentDirectory(settings agent.Settings, cwd string) string {
	if dir := settings.AttachmentDir; dir != "" {
		return resolvePath(cwd, dir)
	}
	return filepath.Join(dashyHome(), "attachments")
}

// dashyHome is $DASHY_HOME, or ~/.dashy when unset.
func dashyHome() string {
	if home := os.Getenv("DASHY_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dashy"
	}
	return filepath.Join(home, ".dashy")
}

func readyRecord(c *control.Controller, cwd string) map[string]any {
	resp, _ := c.Submit(control.Request{Type: "state"})
	return map[string]any{
		"type":     "ready",
		"protocol": protocolVersion,
		"version":  strings.TrimPrefix(tui.Version, "v"),
		"cwd":      cwd,
		"state":    resp.Data,
		"capabilities": map[string]any{
			"requests":         control.Types,
			"max_record_bytes": maxRecordBytes,
			"max_images":       maxImagesPerInput,
			"max_file_bytes":   c.Settings().MaxFileBytes,
		},
	}
}

// inputRecord is one framed stdin line; tooLong marks a record that was discarded.
type inputRecord struct {
	line    []byte
	tooLong bool
}

// readRecords frames newline-delimited records without buffering more than
// maxRecordBytes of one line. An oversized line is reported and skipped, so the
// stream stays in sync for the next request.
func readRecords(in io.Reader, out chan<- inputRecord, stop <-chan struct{}) {
	defer close(out)
	reader := bufio.NewReaderSize(in, 64*1024)
	for {
		line, tooLong, err := readRecord(reader)
		if len(bytes.TrimSpace(line)) > 0 || tooLong {
			select {
			case out <- inputRecord{line: line, tooLong: tooLong}:
			case <-stop:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// readRecord returns the next line without its terminator. Bytes beyond the
// limit are discarded while the rest of the line is consumed.
func readRecord(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	tooLong := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxRecordBytes+2 {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > maxRecordBytes {
			tooLong, line = true, nil
		}
		return line, tooLong, err
	}
}

// joinWithin waits for done to close, for at most grace. It reports whether done closed.
func joinWithin(done <-chan struct{}, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// inputReader owns the goroutine that frames stdin. Go cannot interrupt a
// goroutine blocked in a read, so closeReader unblocks a read only when the
// input supports deadlines (pipes from os.Pipe, network connections). os.Stdin
// is usually a plain descriptor without deadlines. There the goroutine stays
// blocked until the next read completes, and that read's record is discarded.
// The process exits soon after, so nothing else reads stdin; callers must not
// read it again after serveJSON returns.
type inputReader struct {
	records  chan inputRecord
	stop     chan struct{}
	done     chan struct{} // closed when the goroutine returns
	in       io.Reader
	stopOnce sync.Once
}

func startReader(in io.Reader) *inputReader {
	r := &inputReader{records: make(chan inputRecord), stop: make(chan struct{}), done: make(chan struct{}), in: in}
	go func() {
		defer close(r.done)
		readRecords(in, r.records, r.stop)
	}()
	return r
}

// closeReader signals the goroutine. With a deadline-capable input it unblocks a
// pending read and waits up to grace for the goroutine to return. Otherwise it
// does not wait at all: a blocked read could not be interrupted, and waiting
// would hold up shutdown for the whole grace period on an idle terminal. It
// reports whether the goroutine had already returned.
func (r *inputReader) closeReader(grace time.Duration) bool {
	r.stopOnce.Do(func() { close(r.stop) })
	if d, ok := r.in.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Now())
		return joinWithin(r.done, grace)
	}
	return joinWithin(r.done, 0)
}

// inFlight tracks request ids whose results are still pending, and how many model listings run.
type inFlight struct {
	mu       sync.Mutex
	ids      map[string]bool
	listings int
	quiet    chan struct{} // closed while no listing is in flight
}

func newInFlight() *inFlight {
	quiet := make(chan struct{})
	close(quiet)
	return &inFlight{ids: map[string]bool{}, quiet: quiet}
}

// begin reserves id; it fails for a duplicate id or when too many listings run.
func (f *inFlight) begin(id string, listing bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids[id] || (listing && f.listings >= maxAsyncRequests) {
		return false
	}
	f.ids[id] = true
	if listing {
		if f.listings == 0 {
			f.quiet = make(chan struct{})
		}
		f.listings++
	}
	return true
}

func (f *inFlight) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ids[id]
}

// end releases id. Call it after the result has been written, so that waiting for
// listings means their results are out.
func (f *inFlight) end(id string, listing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ids, id)
	if listing {
		f.listings--
		if f.listings == 0 {
			close(f.quiet)
		}
	}
}

// wait blocks until no listing is in flight or ctx ends. It reports ctx's error in the latter case.
func (f *inFlight) wait(ctx context.Context) error {
	f.mu.Lock()
	quiet := f.quiet
	f.mu.Unlock()
	select {
	case <-quiet:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// transport is the single writer of stdout. Emission never blocks: a full
// backlog is reported through overflow so the controller can stop safely.
type transport struct {
	out       io.Writer
	mu        sync.Mutex
	seq       int
	closed    bool
	ended     bool
	err       error
	ch        chan map[string]any
	done      chan struct{}
	broken    chan struct{}
	overflow  chan struct{}
	closeOnce sync.Once
}

func newTransport(out io.Writer) *transport {
	t := &transport{
		out:      out,
		ch:       make(chan map[string]any, outboxLimit),
		done:     make(chan struct{}),
		broken:   make(chan struct{}),
		overflow: make(chan struct{}, 1),
	}
	go t.write()
	return t
}

// write encodes records in order; after a write failure it drains the backlog silently.
func (t *transport) write() {
	defer close(t.done)
	enc := json.NewEncoder(t.out)
	enc.SetEscapeHTML(false)
	failed := false
	for record := range t.ch {
		if !failed {
			if err := enc.Encode(record); err != nil {
				failed = true
				t.mu.Lock()
				t.err = err
				t.mu.Unlock()
				close(t.broken)
			}
		}
	}
}

func (t *transport) failure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// emit queues one protocol line, assigning the next sequence number.
func (t *transport) emit(v any) {
	record := toRecord(v)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.seq++
	record["seq"] = t.seq
	select {
	case t.ch <- record:
	default:
		t.seq--
		select {
		case t.overflow <- struct{}{}:
		default:
		}
	}
}

// close stops accepting records and waits up to flushGrace for the writer to flush.
// A writer still blocked in Write is unblocked through SetWriteDeadline when the
// output supports deadlines; otherwise it stays blocked until that Write returns
// (see inputReader for the same limit on stdin). Only the first call does this work.
func (t *transport) close() {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		close(t.ch)
		t.mu.Unlock()
		if joinWithin(t.done, flushGrace) {
			return
		}
		if d, ok := t.out.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = d.SetWriteDeadline(time.Now())
			joinWithin(t.done, flushGrace)
		}
	})
}

// finish writes the shutdown record once and closes the transport.
func (t *transport) finish(reason string) {
	t.mu.Lock()
	ended := t.ended
	t.ended = true
	t.mu.Unlock()
	if !ended {
		t.emit(map[string]any{"type": "shutdown", "reason": reason})
	}
	t.close()
}

// result answers one request: a status and payload on success, a code and message otherwise.
func (t *transport) result(id string, resp control.Response, err error) {
	record := map[string]any{"type": "result", "id": id}
	if err != nil {
		record["ok"] = false
		record["error"] = errorBody(err)
		t.emit(record)
		return
	}
	record["ok"] = true
	record["status"] = resp.Status
	if resp.Turn != "" {
		record["turn"] = resp.Turn
	}
	if resp.Input != nil {
		record["input"] = wireInput(*resp.Input)
	}
	if resp.Data != nil {
		record["data"] = resp.Data
	}
	t.emit(record)
}

func (t *transport) protocolError(code, message string) {
	t.emit(map[string]any{"type": "error", "error": map[string]string{"code": code, "message": message}})
}

// errorBody reports a rejection without credentials; unexpected errors use a generic code.
func errorBody(err error) map[string]string {
	var e *control.Error
	if errors.As(err, &e) {
		return map[string]string{"code": e.Code, "message": e.Message}
	}
	return map[string]string{"code": "failed", "message": err.Error()}
}

// wireInput names an input by its text and by the paths of its snapshots, so a
// client can resubmit it.
func wireInput(in agent.Input) map[string]any {
	images, files := []string{}, []string{}
	for _, image := range in.Images {
		images = append(images, image.Path)
	}
	for _, file := range in.Files {
		files = append(files, file.Path)
	}
	return map[string]any{"text": in.Text, "images": images, "files": files}
}

// toRecord converts controller values into protocol records.
func toRecord(v any) map[string]any {
	if record, ok := v.(map[string]any); ok {
		return record
	}
	e, ok := v.(control.Event)
	if !ok {
		return map[string]any{"type": "error", "error": map[string]string{"code": "internal", "message": fmt.Sprint(v)}}
	}
	switch e.Kind {
	case "started":
		return map[string]any{"type": "started", "turn": e.Turn, "operation": e.Operation, "source": e.Source}
	case "agent":
		return map[string]any{"type": "event", "turn": e.Turn, "event": e.Agent}
	case "notice":
		return map[string]any{"type": "notice", "turn": e.Turn, "text": e.Text}
	case "error":
		return map[string]any{"type": "error", "turn": e.Turn, "error": map[string]string{"code": "failed", "message": e.Text}}
	case "done":
		unsent := []map[string]any{}
		for _, in := range e.Unsent {
			unsent = append(unsent, wireInput(in))
		}
		return map[string]any{"type": "done", "turn": e.Turn, "operation": e.Operation, "status": e.Status,
			"error": e.Error, "message": e.Message, "unsent": unsent}
	case "session":
		return map[string]any{"type": "session", "session": e.Session}
	}
	return map[string]any{"type": "error", "error": map[string]string{"code": "internal", "message": "unknown event " + e.Kind}}
}
