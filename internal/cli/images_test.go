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
					err = json.NewEncoder(conn).Encode(map[string]string{"error": status})
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
