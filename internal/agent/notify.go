package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dashy/internal/shell"
)

// Message is one request on the session socket: notification text or an image
// queued by dashy view for the next model request.
type Message struct {
	Text  string       `json:"text,omitempty"`
	Image *shell.Image `json:"image,omitempty"`
}

// SocketPath returns the unix socket path for the given session ID.
// If the path exceeds the macOS limit of 104 bytes, a deterministic short hash is used.
func SocketPath(runDir, id string) string {
	candidate := filepath.Join(runDir, id+".sock")
	if len(candidate) < 104 {
		return candidate
	}
	hash := sha256.Sum256([]byte(runDir + "/" + id))
	shortName := hex.EncodeToString(hash[:8]) + ".sock"
	candidate = filepath.Join(runDir, shortName)
	if len(candidate) < 104 {
		return candidate
	}
	// When runDir itself is too long to host any unix socket (e.g. long temp paths on macOS),
	// use /tmp with a deterministic hash of runDir and id.
	return filepath.Join("/tmp", "dashy-"+shortName)
}

func (a *Agent) listenNotifications() error {
	runDir := filepath.Join(a.home, "run")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	sockPath := SocketPath(runDir, a.sessionID())

	// Check if a stale socket exists and remove it if no active listener answers.
	if conn, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("session socket %s is in use", sockPath)
	} else {
		_ = os.Remove(sockPath)
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(sockPath, 0600)
	a.listener, a.socketPath = listener, sockPath
	a.inbox, a.views = make(chan string, 64), make(chan shell.Image, 4)
	inbox, views := a.inbox, a.views
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
			var message Message
			err = json.NewDecoder(io.LimitReader(connection, 4*1024*1024)).Decode(&message)
			status := ""
			var escaped bytes.Buffer
			switch {
			case err != nil:
				status = err.Error()
			case message.Image != nil:
				if !strings.HasPrefix(message.Image.URL, "data:image/") {
					status = "invalid image"
					break
				}
				select {
				case views <- *message.Image:
				default:
					status = "view queue is full: four images per model request"
				}
			case strings.TrimSpace(message.Text) == "" || len(message.Text) > 128*1024:
				status = "notify requires 1..131072 bytes of message text"
			default:
				if err = xml.EscapeText(&escaped, []byte(message.Text)); err != nil {
					status = err.Error()
					break
				}
				select {
				case inbox <- "<notify>\n" + escaped.String() + "\n</notify>":
				default:
					status = "notification queue is full"
				}
			}
			_ = json.NewEncoder(connection).Encode(status)
			_ = connection.Close()
		}
	}()
	return nil
}

func (a *Agent) Notifications() []string {
	var messages []string
	for {
		select {
		case text := <-a.inbox:
			messages = append(messages, text)
		default:
			return messages
		}
	}
}

func (a *Agent) Inbox() <-chan string { return a.inbox }

// viewed drains images queued by dashy view.
func (a *Agent) viewed() []shell.Image {
	var images []shell.Image
	for {
		select {
		case image := <-a.views:
			images = append(images, image)
		default:
			return images
		}
	}
}

// Endpoint resolves a session socket. Without an ID it uses DASHY_ENDPOINT,
// which every dashy shell exports.
func Endpoint(cwd, id string) (string, error) {
	if id == "" {
		if endpoint := os.Getenv("DASHY_ENDPOINT"); endpoint != "" {
			return endpoint, nil
		}
		return "", fmt.Errorf("outside a dashy shell, pass -s SESSION_ID")
	}
	if filepath.Base(id) != id {
		return "", fmt.Errorf("invalid session ID: %s", id)
	}
	home, err := DashyHome(cwd)
	if err != nil {
		return "", err
	}
	sockPath := SocketPath(filepath.Join(home, "run"), id)
	if _, err := os.Stat(sockPath); err != nil {
		return "", fmt.Errorf("session is inactive: %s", id)
	}
	return sockPath, nil
}

// Notify acknowledges enqueueing by the active owner, not acceptance by the model.
// The owner validates the text size.
func Notify(cwd, id, text string) error {
	endpoint, err := Endpoint(cwd, id)
	if err != nil {
		return err
	}
	return Send(endpoint, Message{Text: text})
}

func Send(endpoint string, message Message) error {
	connection, err := net.DialTimeout("unix", endpoint, 2*time.Second)
	if err != nil {
		return fmt.Errorf("session is inactive: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if err = json.NewEncoder(connection).Encode(message); err != nil {
		return err
	}
	var status string
	if err = json.NewDecoder(connection).Decode(&status); err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("%s", status)
	}
	return nil
}

// CleanAttachments deletes attachment files older than 7 days from dir.
func CleanAttachments(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
	return nil
}
