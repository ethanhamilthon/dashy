package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type Message struct {
	Image *shell.Image `json:"image,omitempty"`
	Task  *TaskRequest `json:"task,omitempty"`
}

type TaskRequest struct {
	Op      string `json:"op"`
	Command string `json:"command,omitempty"`
	ID      string `json:"id,omitempty"`
}

type reply struct {
	Error string `json:"error,omitempty"`
	Text  string `json:"text,omitempty"`
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
	// When runDir is too long for a unix socket (long macOS temp paths), use /tmp with a hash.
	return filepath.Join("/tmp", "dashy-"+shortName)
}

type sessionSocket struct {
	path     string
	listener net.Listener
	queue    *Steering
	slots    *imageSlots
	tasks    *tasks
	done     chan struct{}
	mu       sync.Mutex
	retired  bool
	current  net.Conn
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
	s := &sessionSocket{path: sockPath, listener: listener, queue: a.queue, slots: a.slots, tasks: a.tasks, done: make(chan struct{})}
	a.sock = s
	go func() {
		defer close(s.done)
		for connection, err := listener.Accept(); err == nil; connection, err = listener.Accept() {
			s.serve(connection)
		}
	}()
	return nil
}

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

func (s *sessionSocket) admit(message Message, err error) reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.retired:
		return reply{Error: "session has ended"}
	case err != nil:
		return reply{Error: err.Error()}
	case message.Image != nil:
		switch {
		case !validImage(*message.Image):
			return reply{Error: "view requires a PNG or JPEG image"}
		case !s.slots.reserve(1):
			return reply{Error: fmt.Sprintf("view: at most %d new images per model request", MaxPendingImages)}
		}
		if !s.queue.Push(Input{Text: "<view>\nImage queued by dashy view.\n</view>", Images: []shell.Image{*message.Image}, Notice: true}) {
			s.slots.release(1)
			return reply{Error: "input queue is full"}
		}
		return reply{}
	case message.Task != nil && message.Task.Op == "run":
		id, err := s.tasks.run(message.Task.Command)
		if err != nil {
			return reply{Error: err.Error()}
		}
		return reply{Text: id}
	case message.Task != nil && message.Task.Op == "stop":
		if err := s.tasks.stop(message.Task.ID); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{}
	}
	return reply{Error: "unknown request"}
}

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

type imageSlots struct {
	mu        sync.Mutex
	reserved  int
	committed int
}

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

func (s *imageSlots) commit(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved = max(0, s.reserved-n)
	s.committed += n
}

func (s *imageSlots) sent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.committed = 0
}

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

func RunTask(cwd, session, command string) (string, error) {
	endpoint, err := Endpoint(cwd, session)
	if err != nil {
		return "", err
	}
	return Send(endpoint, Message{Task: &TaskRequest{Op: "run", Command: command}})
}

func Send(endpoint string, message Message) (string, error) {
	connection, err := net.DialTimeout("unix", endpoint, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("session is inactive: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if err = json.NewEncoder(connection).Encode(message); err != nil {
		return "", err
	}
	var answer reply
	if err = json.NewDecoder(connection).Decode(&answer); err == nil && answer.Error != "" {
		err = errors.New(answer.Error)
	}
	return answer.Text, err
}

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
