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

// Message is one session socket request: notification text or an image for the next model request.
type Message struct {
	Text  string       `json:"text,omitempty"`
	Image *shell.Image `json:"image,omitempty"`
}

// MaxNotifyBytes is the largest notification text a session accepts.
const MaxNotifyBytes = 128 * 1024

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
	// When runDir is too long for a unix socket (long macOS temp paths), use /tmp with a hash.
	return filepath.Join("/tmp", "dashy-"+shortName)
}

// sessionSocket is one session's notification socket. Its handlers never reference the Agent.
type sessionSocket struct {
	path     string
	listener net.Listener
	queue    *Steering
	slots    *imageSlots
	done     chan struct{} // closed when the accept loop has returned
	mu       sync.Mutex    // guards retired and current, and admission
	retired  bool
	current  net.Conn // connection being read; retire closes it
}

func (a *Agent) listenNotifications() error {
	runDir := filepath.Join(a.home, "run")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	sockPath := SocketPath(runDir, a.SessionID())

	// The session lock is held, so no live listener owns this path: a file left by a crash is stale.
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(sockPath, 0600)
	s := &sessionSocket{path: sockPath, listener: listener, queue: a.queue, slots: a.slots, done: make(chan struct{})}
	a.sock = s
	go func() {
		defer close(s.done)
		for connection, err := listener.Accept(); err == nil; connection, err = listener.Accept() {
			s.serve(connection)
		}
	}()
	return nil
}

// serve answers one connection; admit refuses it if the session was retired meanwhile.
func (s *sessionSocket) serve(connection net.Conn) {
	defer connection.Close()
	s.mu.Lock()
	s.current = connection
	s.mu.Unlock()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	var message Message
	err := json.NewDecoder(io.LimitReader(connection, 4*1024*1024)).Decode(&message)
	_ = json.NewEncoder(connection).Encode(s.admit(message, err))
}

// admit queues a request if the session is still current and returns the status to reply.
func (s *sessionSocket) admit(message Message, err error) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired {
		return "session has ended"
	}
	if err != nil {
		return err.Error()
	}
	if message.Image != nil {
		switch {
		case !validImage(*message.Image):
			return "view requires a PNG or JPEG image"
		case !s.slots.reserve(1):
			return fmt.Sprintf("view: at most %d new images per model request", MaxPendingImages)
		}
		// The image joins the input queue like a notification; it reaches the next model request.
		if !s.queue.Push(Input{Text: "<notify>\nImage queued by dashy view.\n</notify>", Images: []shell.Image{*message.Image}, Notice: true}) {
			s.slots.release(1)
			return "notification queue is full"
		}
		return ""
	}
	if strings.TrimSpace(message.Text) == "" || len(message.Text) > MaxNotifyBytes {
		return fmt.Sprintf("notify requires 1..%d bytes of message text", MaxNotifyBytes)
	}
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(message.Text)) // writes to a bytes.Buffer never fail
	if !s.queue.Push(Input{Text: "<notify>\n" + escaped.String() + "\n</notify>", Notice: true}) {
		return "notification queue is full"
	}
	return ""
}

// retire stops the socket before its session is replaced; no handler runs once it returns.
func (s *sessionSocket) retire() {
	s.mu.Lock()
	s.retired = true
	if s.current != nil {
		s.current.Close()
	}
	s.mu.Unlock()
	s.listener.Close()
	<-s.done
}

// imageSlots counts the images of the next model request: reserved from admission until they
// are in the history, then committed until a request carrying the history is sent.
type imageSlots struct {
	mu        sync.Mutex
	reserved  int // admitted, not yet in the history
	committed int // in the history, not yet carried by a model request
}

// MaxPendingImages is how many new images one model request may carry.
const MaxPendingImages = 4

func (s *imageSlots) reserve(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserved+s.committed+n > MaxPendingImages {
		return false
	}
	s.reserved += n
	return true
}

func (s *imageSlots) release(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved = max(0, s.reserved-n)
}

// commit moves n admitted images into the history, where they wait for the next request.
func (s *imageSlots) commit(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved = max(0, s.reserved-n)
	s.committed += n
}

// sent runs when a request carrying the history is dispatched; those images are no longer new.
func (s *imageSlots) sent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.committed = 0
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

// Endpoint resolves a session socket; without an ID it uses DASHY_ENDPOINT, which every dashy shell exports.
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

// Notify queues text for the active owner, which validates its size.
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
	if err = json.NewDecoder(connection).Decode(&status); err == nil && status != "" {
		err = fmt.Errorf("%s", status)
	}
	return err
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
