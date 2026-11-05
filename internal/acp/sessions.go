package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dashy/internal/agent"
)

const listPage = 50

// list pages through saved sessions, newest first. The opaque cursor is the last
// returned session's update time and ID, so new activity never shifts a page.
func (s *server) list(params json.RawMessage, reply replyFunc) {
	var p struct {
		Cwd    *string `json:"cwd"`
		Cursor *string `json:"cursor"`
	}
	if len(params) > 0 && string(params) != "null" && json.Unmarshal(params, &p) != nil {
		reply(nil, problem(-32602, "Invalid params"))
		return
	}
	project := ""
	if p.Cwd != nil {
		if !filepath.IsAbs(*p.Cwd) {
			reply(nil, problem(-32602, "cwd must be an absolute path"))
			return
		}
		project = filepath.Clean(*p.Cwd)
	}
	var after agent.SessionSummary
	if p.Cursor != nil {
		nanos, id, _ := strings.Cut(*p.Cursor, ":")
		n, err := strconv.ParseInt(nanos, 10, 64)
		if err != nil || id == "" {
			reply(nil, problem(-32602, "Invalid cursor"))
			return
		}
		after = agent.SessionSummary{ID: id, Updated: time.Unix(0, n)}
	}
	s.job(func() {
		cwd, _ := os.Getwd()
		sessions, err := agent.ListSessions(cwd, project, after, listPage+1)
		if err != nil {
			reply(nil, problem(-32603, "Unable to read local session storage"))
			return
		}
		infos := []map[string]any{}
		for i, summary := range sessions {
			if i == listPage {
				break
			}
			info := map[string]any{"sessionId": summary.ID, "cwd": summary.Project, "updatedAt": summary.Updated.UTC().Format(time.RFC3339Nano)}
			if summary.Title != "" {
				info["title"] = summary.Title
			}
			infos = append(infos, info)
		}
		result := map[string]any{"sessions": infos}
		if len(sessions) > listPage {
			last := sessions[listPage-1]
			result["nextCursor"] = strconv.FormatInt(last.Updated.UnixNano(), 10) + ":" + last.ID
		}
		reply(result, nil)
	})
}

// closeSession cancels any running work, waits for it, and releases the session lock.
func (s *server) closeSession(params json.RawMessage, reply replyFunc) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if !decode(params, &p) || p.SessionID == "" {
		reply(nil, problem(-32602, "sessionId is required"))
		return
	}
	s.mu.Lock()
	session := s.sessions[p.SessionID]
	delete(s.sessions, p.SessionID)
	s.mu.Unlock()
	if session == nil {
		reply(nil, problem(-32002, "Unknown session"))
		return
	}
	session.mu.Lock()
	session.closed = true
	if session.cancel != nil {
		session.cancelled = true
		session.cancel()
	}
	session.mu.Unlock()
	s.job(func() {
		session.work.Wait()
		session.a.Close()
		reply(map[string]any{}, nil)
	})
}
