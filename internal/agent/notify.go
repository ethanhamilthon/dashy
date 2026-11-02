package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "image/jpeg" // registers the decoder used by validImage
	_ "image/png"

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
	sockPath := SocketPath(runDir, a.SessionID())

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
				switch {
				case !validImage(*message.Image):
					status = "view requires a PNG or JPEG image"
				case !a.ReserveImages(1):
					status = "view: at most four new images per model request"
				default:
					views <- *message.Image // the reservation bounds the queue to its capacity
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

// imageSlots counts the images of the next model request. An image is reserved from
// admission (composer, steering, dashy view) until it is in the history, and committed
// from then until a model request carrying the history is sent. Both count against
// MaxPendingImages, so admissions made while a request is in flight or waiting to be
// sent cannot push the next request past the limit.
type imageSlots struct {
	mu        sync.Mutex
	reserved  int // admitted, not yet in the history
	committed int // in the history, not yet carried by a model request
}

// MaxPendingImages is how many new images one model request may carry.
const MaxPendingImages = 4

// ReserveImages admits n images, or reports false when they would exceed MaxPendingImages.
func (a *Agent) ReserveImages(n int) bool {
	a.slots.mu.Lock()
	defer a.slots.mu.Unlock()
	if a.slots.reserved+a.slots.committed+n > MaxPendingImages {
		return false
	}
	a.slots.reserved += n
	return true
}

// ReleaseImages drops n admitted images that will not reach the history (returned unsent or recalled).
func (a *Agent) ReleaseImages(n int) {
	a.slots.mu.Lock()
	defer a.slots.mu.Unlock()
	a.slots.reserved = max(0, a.slots.reserved-n)
}

// commitImages moves n admitted images into the history, where they wait for the next request.
func (a *Agent) commitImages(n int) {
	a.slots.mu.Lock()
	defer a.slots.mu.Unlock()
	a.slots.reserved = max(0, a.slots.reserved-n)
	a.slots.committed += n
}

// sentImages runs when a request carrying the history is dispatched: the images
// in the history are no longer new for the requests that follow.
func (a *Agent) sentImages() {
	a.slots.mu.Lock()
	defer a.slots.mu.Unlock()
	a.slots.committed = 0
}

// PendingImages counts the images that the next model request will carry.
func (a *Agent) PendingImages() int {
	a.slots.mu.Lock()
	defer a.slots.mu.Unlock()
	return a.slots.reserved + a.slots.committed
}

// viewed drains images queued by dashy view and commits them to the history.
func (a *Agent) viewed() []shell.Image {
	var images []shell.Image
	for {
		select {
		case image := <-a.views:
			images = append(images, image)
		default:
			a.commitImages(len(images))
			return images
		}
	}
}

// validImage accepts only a PNG or JPEG data URL whose payload decodes as that format.
func validImage(img shell.Image) bool {
	for _, format := range []string{"png", "jpeg"} {
		payload, ok := strings.CutPrefix(img.URL, "data:image/"+format+";base64,")
		if !ok {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return false
		}
		_, decoded, err := image.DecodeConfig(bytes.NewReader(data))
		return err == nil && decoded == format
	}
	return false
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
