package acp

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	"dashy/internal/agent"
)

type configChoice struct {
	Value string `json:"value"`
	Name  string `json:"name"`
}
type configOption struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Category     string         `json:"category"`
	Type         string         `json:"type"`
	CurrentValue string         `json:"currentValue"`
	Options      []configChoice `json:"options"`
}

func modelID(provider, model string) string { return url.PathEscape(provider) + "/" + model }
func effortID(effort string) string {
	if effort == "" {
		return "default"
	}
	return effort
}

func (session *session) discoverModels(ctx context.Context) {
	settings := session.a.Settings()
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	choices, _ := agent.AllModels(listCtx, settings.Providers)
	session.models = map[string]agent.ModelChoice{modelID(settings.Provider, settings.Model): {Provider: settings.Provider, Model: settings.Model}}
	for _, choice := range choices {
		if len(session.models) >= 2000 {
			break
		}
		session.models[modelID(choice.Provider, choice.Model)] = choice
	}
}
func (session *session) configOptions() []configOption {
	settings := session.a.Settings()
	models := make([]configChoice, 0, len(session.models))
	for id, choice := range session.models {
		models = append(models, configChoice{id, choice.Model + " · " + choice.Provider})
	}
	slices.SortFunc(models, func(a, b configChoice) int { return strings.Compare(a.Value, b.Value) })
	efforts := make([]configChoice, 0, len(agent.Efforts))
	for _, effort := range agent.Efforts {
		name := effort
		if name == "" {
			name = "Provider default"
		}
		efforts = append(efforts, configChoice{effortID(effort), name})
	}
	return []configOption{
		{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: modelID(settings.Provider, settings.Model), Options: models},
		{ID: "effort", Name: "Reasoning effort", Category: "thought_level", Type: "select", CurrentValue: effortID(settings.Effort), Options: efforts},
	}
}
func (s *server) setConfig(ctx context.Context, params []byte, reply replyFunc) {
	var p struct {
		SessionID string  `json:"sessionId"`
		ConfigID  string  `json:"configId"`
		Value     *string `json:"value"`
	}
	if !decode(params, &p) || p.SessionID == "" || p.Value == nil {
		reply(nil, problem(-32602, "sessionId, configId and a string value are required"))
		return
	}
	session := s.lookup(p.SessionID)
	if session == nil {
		reply(nil, problem(-32002, "Unknown session"))
		return
	}
	session.mu.Lock()
	if session.busy {
		session.mu.Unlock()
		reply(nil, problem(-32600, "Session is busy; change configuration between turns"))
		return
	}
	settings := session.a.Settings()
	switch p.ConfigID {
	case "model":
		choice, ok := session.models[*p.Value]
		if !ok {
			session.mu.Unlock()
			reply(nil, problem(-32602, "Unknown model choice"))
			return
		}
		settings.Provider, settings.Model = choice.Provider, choice.Model
	case "effort":
		effort := *p.Value
		if effort == "default" {
			effort = ""
		}
		if *p.Value == "" || !slices.Contains(agent.Efforts, effort) {
			session.mu.Unlock()
			reply(nil, problem(-32602, "Unknown reasoning effort"))
			return
		}
		settings.Effort = effort
	default:
		session.mu.Unlock()
		reply(nil, problem(-32602, "Unknown configuration option"))
		return
	}
	session.busy = true
	session.mu.Unlock()
	s.job(func() {
		err := session.a.SelectSession(ctx, settings.Provider, settings.Model, settings.Effort)
		session.mu.Lock()
		defer session.mu.Unlock()
		session.busy = false
		if err != nil {
			code := -32603
			if ctx.Err() != nil {
				code = -32800
			}
			reply(nil, problem(code, agent.Redact(session.a.Settings(), err.Error())))
			return
		}
		options := session.configOptions()
		s.update(session, map[string]any{"sessionUpdate": "config_option_update", "configOptions": options})
		s.usage(session)
		reply(map[string]any{"configOptions": options}, nil)
	})
}
