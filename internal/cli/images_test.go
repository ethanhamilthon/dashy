package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashy/internal/agent"
)

func TestViewQueuesPreviewAndOriginal(t *testing.T) {
	for _, status := range []string{"", "view queue is full"} {
		t.Run(status, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("DASHY_HOME", home)
			// Keep Unix socket paths below the platform's length limit.
			dir, err := os.MkdirTemp("", "rv-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			endpoint := filepath.Join(dir, "s")
			t.Setenv("DASHY_ENDPOINT", endpoint)
			listener, err := net.Listen("unix", endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			messages := make(chan agent.Message, 1)
			errors := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					errors <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				var message agent.Message
				if err = json.NewDecoder(conn).Decode(&message); err == nil {
					err = json.NewEncoder(conn).Encode(status)
				}
				errors <- err
				messages <- message
			}()
			var original bytes.Buffer
			if err = png.Encode(&original, image.NewNRGBA(image.Rect(0, 0, 3290, 1170))); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "sheet.png")
			if err = os.WriteFile(path, original.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			err = Run(context.Background(), []string{"view", path}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
			if (err != nil) != (status != "") {
				t.Fatalf("view status: %v", err)
			}
			if err = <-errors; err != nil {
				t.Fatal(err)
			}
			message := <-messages
			if message.Image == nil {
				t.Fatal("image missing from socket message")
			}
			_, encoded, _ := strings.Cut(message.Image.URL, ";base64,")
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || cfg.Width != 2048 || cfg.Height != 728 || len(data) > 1024*1024 {
				t.Fatalf("wrong preview: %+v, %v", cfg, err)
			}
			saved, err := os.ReadFile(message.Image.Path)
			if status == "" {
				if err != nil || !bytes.Equal(saved, original.Bytes()) || filepath.Dir(message.Image.Path) != filepath.Join(home, "attachments") {
					t.Fatal("original snapshot missing or modified", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("rejected image left an unused snapshot", err)
			}
		})
	}
}

// TestJSONImageInputSnapshotsAndRejectsInvalid covers image attachments sent as
// stdin records: the model receives the image, the snapshot is kept for later
// crops, and invalid or excessive attachments are rejected before any turn starts.
func TestJSONImageInputSnapshotsAndRejectsInvalid(t *testing.T) {
	p := newProvider(t, nil)
	home := p.configure(t, "")
	project := t.TempDir()
	t.Chdir(project)
	var png bytes.Buffer
	if err := encodeTestPNG(&png); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "sheet.png"), png.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	c := startJSON(t)
	c.send(`{"id":"i1","type":"input","text":"look","images":["sheet.png"]}`)
	turn := str(c.await("started", resultFor("i1")), "turn")
	c.await("done", doneFor(turn))
	bodies := p.requestBodies()
	if len(bodies) == 0 || !strings.Contains(bodies[0], `"input_image"`) || !strings.Contains(bodies[0], "data:image/png;base64,") {
		t.Fatalf("image not sent to the model: %v", bodies)
	}
	if snapshots, _ := filepath.Glob(filepath.Join(home, "attachments", "original-*.png")); len(snapshots) != 1 {
		t.Fatalf("original snapshot missing: %v", snapshots)
	}
	c.send(`{"id":"i2","type":"input","text":"bad","images":["notes.txt"]}`)
	if res := c.await("invalid image", resultFor("i2")); res["error"].(map[string]any)["code"] != "invalid_image" {
		t.Fatalf("text file accepted as image: %v", res)
	}
	c.send(`{"id":"i3","type":"input","text":"many","images":["sheet.png","sheet.png","sheet.png","sheet.png","sheet.png","sheet.png","sheet.png","sheet.png","sheet.png"]}`)
	if res := c.await("too many", resultFor("i3")); res["error"].(map[string]any)["code"] != "attachment_limit" {
		t.Fatalf("nine images accepted: %v", res)
	}
	if got := len(p.requestBodies()); got != 1 {
		t.Fatalf("rejected inputs reached the model: %d requests", got)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func encodeTestPNG(out *bytes.Buffer) error {
	return png.Encode(out, image.NewNRGBA(image.Rect(0, 0, 4, 4)))
}
