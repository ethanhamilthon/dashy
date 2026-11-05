package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashy/internal/shell"
)

func pngURL() string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG())
}

// The session socket refuses empty requests, empty task commands and images that do not decode.
func TestSocketValidates(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	for _, m := range []Message{
		{},
		{Task: &TaskRequest{Op: "run", Command: " "}},
		{Task: &TaskRequest{Op: "stop", ID: "t9"}},
		{Image: &shell.Image{URL: "data:image/svg+xml;base64,PHN2Zy8+"}},
		{Image: &shell.Image{URL: "data:image/png;base64,bm90IGFuIGltYWdl"}},
	} {
		if _, err := Send(a.sock.path, m); err == nil {
			t.Fatalf("accepted %+v", m)
		}
	}
	if got := a.Queue().Take(); len(got) != 0 {
		t.Fatalf("queued %+v", got)
	}
}

// awaitInput waits until the session queue holds input and takes it.
func awaitInput(t *testing.T, a *Agent) []Input {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got := a.Queue().Take(); len(got) > 0 {
			return got
		}
	}
	t.Fatal("no input arrived")
	return nil
}

// A task runs in the project directory with the session's variables (its endpoint too),
// is listed while it runs, and its result (escaped command and output, exit code) arrives
// as a notice. A nonzero exit is failed.
func TestTaskRunsAndReportsItsResult(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	id, err := Send(a.sock.path, Message{Task: &TaskRequest{Op: "run", Command: `echo "<$DASHY_SESSION_ID>" "[$DASHY_ENDPOINT]" && pwd && exit 3`}})
	if err != nil || id != "t1" {
		t.Fatalf("run: %q %v", id, err)
	}
	got := awaitInput(t, a)
	text := got[0].Text
	if len(got) != 1 || !got[0].Notice || !strings.HasPrefix(text, `<task-result id="t1" status="failed" exit_code="3"`) ||
		!strings.Contains(text, "&lt;"+a.SessionID()+"&gt; ["+a.sock.path+"]") || !strings.Contains(text, a.Shell.Dir) || !strings.Contains(text, `$ echo "&lt;$DASHY_SESSION_ID&gt;"`) {
		t.Fatalf("result %+v", got)
	}
	if s := NoticeSummary(text); !strings.HasPrefix(s, "task t1 failed · exit 3 · ") || !strings.HasSuffix(s, `$ echo "<$DASHY_SESSION_ID>" "[$DASHY_ENDPOINT]" && pwd && exit 3`) {
		t.Fatalf("summary %q", s)
	}
	list, err := SessionTasks(a.home, a.SessionID())
	if err != nil || len(list) != 1 || list[0].Status != "failed" || *list[0].ExitCode != 3 {
		t.Fatalf("list %+v %v", list, err)
	}
}

// A stopped task reports stopped; closing the session stops running tasks without a result.
func TestTaskStopAndSessionClose(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if _, err := Send(a.sock.path, Message{Task: &TaskRequest{Op: "run", Command: "sleep 30"}}); err != nil {
		t.Fatal(err)
	}
	if a.RunningTasks() != 1 {
		t.Fatal("task not running")
	}
	if _, err := Send(a.sock.path, Message{Task: &TaskRequest{Op: "stop", ID: "t1"}}); err != nil {
		t.Fatal(err)
	}
	if got := awaitInput(t, a); !strings.HasPrefix(got[0].Text, `<task-result id="t1" status="stopped"`) {
		t.Fatalf("result %+v", got)
	}
	if _, err := Send(a.sock.path, Message{Task: &TaskRequest{Op: "run", Command: "sleep 30"}}); err != nil {
		t.Fatal(err)
	}
	old, start := a.SessionID(), time.Now()
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second || a.RunningTasks() != 0 || len(a.Queue().Take()) != 0 {
		t.Fatal("session change did not stop the task quietly")
	}
	if list, err := SessionTasks(a.home, old); err != nil || len(list) != 2 || list[1].Status != "stopped" {
		t.Fatalf("tasks %+v %v", list, err)
	}
}

// Images reserved for a request and dashy view images share one allowance of four per request.
func TestImageAllowanceIsSharedWithView(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	if !a.slots.reserve(2) {
		t.Fatal("composer images refused")
	}
	for i := 0; i < 2; i++ {
		if _, err := Send(a.sock.path, Message{Image: &shell.Image{URL: pngURL()}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Send(a.sock.path, Message{Image: &shell.Image{URL: pngURL()}}); err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("fifth image admitted: %v", err)
	}
	if got := pendingImages(a); got != 4 {
		t.Fatalf("pending images = %d", got)
	}
}

// Images queued by dashy view reach exactly one model request and then stay in history.
func TestViewedImageReachesNextRequestOnce(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "true")
		}
		return textReply("ok")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := Send(a.sock.path, Message{Image: &shell.Image{URL: pngURL()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := turn(t, a, "look"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if strings.Count(reqs[0], "input_image") != 1 || strings.Count(reqs[1], "input_image") != 1 || pendingImages(a) != 0 {
		t.Fatalf("image counts: %d, %d, pending %d", strings.Count(reqs[0], "input_image"), strings.Count(reqs[1], "input_image"), pendingImages(a))
	}
}

// Images are counted from admission until the request that carries them is sent.
// Admissions made after the input is in the history but before that request is sent,
// or while it is in flight, are measured against the same allowance.
func TestImagesInHistoryCountUntilSent(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	images := []shell.Image{{URL: pngURL()}, {URL: pngURL()}, {URL: pngURL()}}
	if !a.slots.reserve(3) {
		t.Fatal("three images refused")
	}
	a.commitInput(Input{Text: "look", Images: images})
	if a.slots.reserve(2) {
		t.Fatal("five images admitted for one request before it was sent")
	}
	if !a.slots.reserve(1) {
		t.Fatal("fourth image refused")
	}
	if _, err := a.request(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(fp.requests()[0], "input_image"); got != 3 {
		t.Fatalf("request carried %d images", got)
	}
	// The fourth image is still queued for the following request: one more slot is free.
	if pendingImages(a) != 1 || a.slots.reserve(4) || !a.slots.reserve(3) {
		t.Fatalf("after sending: pending %d", pendingImages(a))
	}
}

// A connection accepted before /new is retired with its session: the image it completes
// after the switch is refused and never queued in the new session.
func TestConnectionAcceptedBeforeNewSessionIsRetired(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	payload, err := json.Marshal(Message{Image: &shell.Image{URL: pngURL()}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", a.sock.path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(payload[:len(payload)/2]); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		a.sock.mu.Lock()
		accepted := a.sock.current != nil
		a.sock.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connection not accepted")
		}
	}
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write(payload[len(payload)/2:])
	var status string
	if err := json.NewDecoder(conn).Decode(&status); err == nil && status == "" {
		t.Fatal("retired connection was acknowledged")
	}
	if pendingImages(a) != 0 || len(a.Queue().Take()) != 0 {
		t.Fatal("image from the retired connection reached the new session")
	}
}

// A socket file left by a crashed session (also in the /tmp fallback) does not block listening.
func TestStaleSocketIsReplaced(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	path := a.sock.path
	a.sock.retire()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := a.listenNotifications(); err != nil {
		t.Fatalf("stale socket blocked listening: %v", err)
	}
}

func pendingImages(a *Agent) int {
	a.slots.mu.Lock()
	defer a.slots.mu.Unlock()
	return a.slots.reserved + a.slots.committed
}

// The end of a huge log comes from its last bytes only, cut at a line start.
func TestTailOfHugeLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 30); err != nil { // sparse: 1 GiB of zero bytes
		t.Fatal(err)
	}
	file.WriteAt([]byte("\nlast line\n"), 1<<30)
	file.Close()
	if got := tail(path); !strings.HasPrefix(got, "... earlier output") || !strings.HasSuffix(got, "\nlast line") || len(got) > taskResultBytes+40 {
		t.Fatalf("tail %d bytes: %q", len(got), got[max(0, len(got)-60):])
	}
}
