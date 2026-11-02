package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// snapshots lists the image copies kept under the attachment directory of home.
func snapshots(home string) []string {
	files, _ := filepath.Glob(filepath.Join(home, "attachments", "original-*"))
	return files
}

// writeTestPNG saves a small valid PNG at path inside the project directory.
func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	var png bytes.Buffer
	if err := encodeTestPNG(&png); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, png.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

// A FIFO named as an image is rejected before it can block the stdin loop, so
// later requests are still answered. Stop carries images it does not use, so
// it must not process them either.
func TestJSONFIFOImageDoesNotBlockControl(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "")
	t.Chdir(t.TempDir())
	if err := syscall.Mkfifo("pipe.png", 0600); err != nil {
		t.Fatal(err)
	}
	c := startJSON(t)
	c.send(`{"id":"f1","type":"input","text":"look","images":["pipe.png"]}`)
	if res := c.within(5*time.Second, resultFor("f1")); res == nil || res["error"].(map[string]any)["code"] != "invalid_image" {
		t.Fatalf("FIFO image not rejected promptly: %v", res)
	}
	c.send(`{"id":"f2","type":"stop","images":["pipe.png"]}`)
	if res := c.within(5*time.Second, resultFor("f2")); res == nil || res["ok"] != true {
		t.Fatalf("stop with an image not answered promptly: %v", res)
	}
	c.send(`{"id":"f3","type":"state"}`)
	if res := c.within(5*time.Second, resultFor("f3")); res == nil || res["ok"] != true {
		t.Fatalf("state after the FIFO image not answered: %v", res)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

// Copies made for a request are kept when it is accepted, and removed when
// preparation fails partway or the controller rejects the request.
func TestJSONRejectedImageRequestsLeaveNoSnapshots(t *testing.T) {
	p := newProvider(t, nil)
	home := p.configure(t, "")
	project := t.TempDir()
	t.Chdir(project)
	writeTestPNG(t, filepath.Join(project, "sheet.png"))
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	c := startJSON(t)
	c.send(`{"id":"r1","type":"input","text":"two","images":["sheet.png","notes.txt"]}`)
	if res := c.await("invalid second image", resultFor("r1")); res["error"].(map[string]any)["code"] != "invalid_image" {
		t.Fatalf("invalid second image accepted: %v", res)
	}
	if n := len(snapshots(home)); n != 0 {
		t.Fatalf("invalid second image left %d snapshots", n)
	}
	c.send(`{"id":"r2","type":"steer","text":"idle","images":["sheet.png"]}`)
	if res := c.await("not_busy", resultFor("r2")); res["error"].(map[string]any)["code"] != "not_busy" {
		t.Fatalf("steer while idle = %v", res)
	}
	if n := len(snapshots(home)); n != 0 {
		t.Fatalf("not_busy rejection left %d snapshots", n)
	}
	c.send(`{"id":"r3","type":"input","text":"look","images":["sheet.png"]}`)
	turn := str(c.await("started", resultFor("r3")), "turn")
	c.await("done", doneFor(turn))
	if n := len(snapshots(home)); n != 1 {
		t.Fatalf("accepted image has %d snapshots, want 1", n)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

// While compaction runs, input with images is rejected as busy and its copy is
// removed. The compaction summary is held by a gate in front of the stub, so
// the controller stays busy until the test releases it.
func TestJSONBusyImageRejectionLeavesNoSnapshot(t *testing.T) {
	hold := make(chan struct{})
	p := newProvider(t, nil)
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && !bytes.Contains(body, []byte(`"stream":true`)) {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		p.server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	release := releaser(hold)
	t.Cleanup(release)
	home := (&provider{server: gate}).configure(t, "compact_keep_turns = 1\n")
	project := t.TempDir()
	t.Chdir(project)
	writeTestPNG(t, filepath.Join(project, "sheet.png"))
	c := startJSON(t)
	for i, text := range []string{"one", "two"} {
		id := fmt.Sprintf("t%d", i)
		c.send(fmt.Sprintf(`{"id":%q,"type":"input","text":%q}`, id, text))
		turn := str(c.await("started "+id, resultFor(id)), "turn")
		c.await("done "+id, doneFor(turn))
	}
	c.send(`{"id":"k1","type":"compact"}`)
	c.await("compact started", resultFor("k1"))
	c.send(`{"id":"b1","type":"input","text":"busy","images":["sheet.png"]}`)
	if res := c.await("busy", resultFor("b1")); res["error"].(map[string]any)["code"] != "busy" {
		t.Fatalf("image input during compaction = %v", res)
	}
	if n := len(snapshots(home)); n != 0 {
		t.Fatalf("busy rejection left %d snapshots", n)
	}
	release()
	c.await("compact done", func(m map[string]any) bool { return m["type"] == "done" && m["operation"] == "compact" })
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}
