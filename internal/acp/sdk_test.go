package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

type sdkClient struct {
	sdk.Client
	mu      sync.Mutex
	updates []sdk.SessionNotification
}

func (c *sdkClient) SessionUpdate(_ context.Context, p sdk.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, p)
	return nil
}
func (c *sdkClient) RequestPermission(context.Context, sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	return sdk.RequestPermissionResponse{}, errors.New("Dashy must not request permissions")
}

func TestMaintainedSDKClientInteroperability(t *testing.T) {
	settings(t, func(string) string { return answer("SDK hello") })
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), server, server, Options{Version: "test", NoGlobal: true}) }()
	defer func() {
		client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	}()
	callback := &sdkClient{}
	peer := sdk.NewClientSideConnection(callback, client, client)
	peer.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	initialized, err := peer.Initialize(ctx, sdk.InitializeRequest{ProtocolVersion: 1})
	if err != nil || initialized.ProtocolVersion != 1 || !initialized.AgentCapabilities.LoadSession {
		t.Fatalf("initialize: %+v %v", initialized, err)
	}
	dir := t.TempDir()
	session, err := peer.NewSession(ctx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(session.ConfigOptions) != 2 {
		t.Fatal("SDK did not receive model and effort selectors")
	}
	for _, change := range []struct{ id, value string }{{"model", "p/m2"}, {"effort", "high"}} {
		data, _ := json.Marshal(map[string]any{"sessionId": session.SessionId, "configId": change.id, "value": change.value})
		var params sdk.SetSessionConfigOptionRequest
		if err := json.Unmarshal(data, &params); err != nil {
			t.Fatal(err)
		}
		response, err := peer.SetSessionConfigOption(ctx, params)
		if err != nil || len(response.ConfigOptions) != 2 {
			t.Fatalf("config response: %+v %v", response, err)
		}
	}
	result, err := peer.Prompt(ctx, sdk.PromptRequest{SessionId: session.SessionId, Prompt: []sdk.ContentBlock{sdk.TextBlock("hello")}})
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("prompt: %+v %v", result, err)
	}
	loaded, err := peer.LoadSession(ctx, sdk.LoadSessionRequest{SessionId: session.SessionId, Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil || len(loaded.ConfigOptions) != 2 {
		t.Fatalf("load: %+v %v", loaded, err)
	}
	callback.mu.Lock()
	defer callback.mu.Unlock()
	users, answers, usage, configs := 0, 0, 0, 0
	for _, notification := range callback.updates {
		if notification.SessionId != session.SessionId {
			t.Fatal("session association lost")
		}
		u := notification.Update
		if u.UserMessageChunk != nil {
			users++
		}
		if u.AgentMessageChunk != nil {
			answers++
		}
		if u.ConfigOptionUpdate != nil {
			configs++
		}
		if u.UsageUpdate != nil {
			usage++
			if u.UsageUpdate.Size != 1_000_000 || u.UsageUpdate.Used < 0 {
				t.Fatalf("usage: %+v", u.UsageUpdate)
			}
		}
	}
	if users != 1 || answers != 2 || configs != 2 || usage != 5 {
		t.Fatalf("SDK update counts: users=%d answers=%d configs=%d usage=%d", users, answers, configs, usage)
	}
}
