package agent

import (
	"bytes"
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

func (a *Agent) listenNotifications() error {
	dir, err := os.MkdirTemp("", "dashy-notify-")
	if err != nil {
		return err
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "socket"))
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	if err = writePrivate(a.HistoryPath+".endpoint", []byte(filepath.Join(dir, "socket"))); err != nil {
		listener.Close()
		os.RemoveAll(dir)
		return err
	}
	a.listener, a.socketDir, a.inbox, a.views = listener, dir, make(chan string, 64), make(chan shell.Image, 4)
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
	home, err := configDir(cwd)
	if err != nil {
		return "", err
	}
	directory, err := projectDirectory(cwd, home)
	if err != nil {
		return "", err
	}
	endpoint, err := os.ReadFile(filepath.Join(directory, id+".jsonl.endpoint"))
	if err != nil {
		return "", fmt.Errorf("session is inactive: %s", id)
	}
	return string(endpoint), nil
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
