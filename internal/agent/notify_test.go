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

// The session socket accepts only text and PNG/JPEG images that decode, and escapes the text.
func TestSocketValidatesAndQueues(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	for _, m := range []Message{
		{Text: " \n "},
		{Image: &shell.Image{URL: "data:image/svg+xml;base64,PHN2Zy8+"}},
		{Image: &shell.Image{URL: "data:image/png;base64,bm90IGFuIGltYWdl"}},
	} {
		if err := Send(a.sock.path, m); err == nil {
			t.Fatalf("accepted %+v", m)
		}
	}
	if err := Send(a.sock.path, Message{Text: "<check & report>"}); err != nil {
		t.Fatal(err)
	}
	if got := a.Queue().Take(); len(got) != 1 || got[0].Text != "<notify>\n&lt;check &amp; report&gt;\n</notify>" || !got[0].Notice {
		t.Fatalf("queued %+v", got)
	}
}

// Composer images and dashy view images share one allowance of four per request.
func TestImageAllowanceIsSharedWithView(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	if !a.ReserveImages(2) {
		t.Fatal("composer images refused")
	}
	for i := 0; i < 2; i++ {
		if err := Send(a.sock.path, Message{Image: &shell.Image{URL: pngURL()}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Send(a.sock.path, Message{Image: &shell.Image{URL: pngURL()}}); err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("fifth image admitted: %v", err)
	}
	if got := a.PendingImages(); got != 4 {
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
	if err := Send(a.sock.path, Message{Image: &shell.Image{URL: pngURL()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := turn(t, a, "look"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if strings.Count(reqs[0], "input_image") != 1 || strings.Count(reqs[1], "input_image") != 1 || a.PendingImages() != 0 {
		t.Fatalf("image counts: %d, %d, pending %d", strings.Count(reqs[0], "input_image"), strings.Count(reqs[1], "input_image"), a.PendingImages())
	}
}

// Images are counted from admission until the request that carries them is sent.
// Admissions made after the input is in the history but before that request is sent,
// or while it is in flight, are measured against the same allowance.
func TestImagesInHistoryCountUntilSent(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	images := []shell.Image{{URL: pngURL()}, {URL: pngURL()}, {URL: pngURL()}}
	if !a.ReserveImages(3) {
		t.Fatal("three images refused")
	}
	a.commitInput(Input{Text: "look", Images: images})
	if a.ReserveImages(2) {
		t.Fatal("five images admitted for one request before it was sent")
	}
	if !a.ReserveImages(1) {
		t.Fatal("fourth image refused")
	}
	if _, err := a.request(context.Background(), func(Event) {}, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(fp.requests()[0], "input_image"); got != 3 {
		t.Fatalf("request carried %d images", got)
	}
	// The fourth image is still queued for the following request: one more slot is free.
	if a.PendingImages() != 1 || a.ReserveImages(4) || !a.ReserveImages(3) {
		t.Fatalf("after sending: pending %d", a.PendingImages())
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
	if a.PendingImages() != 0 || len(a.viewed()) != 0 {
		t.Fatal("image from the retired connection reached the new session")
	}
}

// Sockets that no listener answers (left by a crashed session) are removed; a live one stays.
func TestDeadSocketsAreRemoved(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "dead.sock")
	l, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	live := filepath.Join(dir, "live.sock")
	ll, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ll.Close()
	removeDeadSockets(dir)
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("dead socket kept: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live socket removed: %v", err)
	}
}
