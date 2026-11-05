package acp

import (
	"context"
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
	if err != nil || initialized.ProtocolVersion != 1 || initialized.AgentCapabilities.LoadSession {
		t.Fatalf("initialize: %+v %v", initialized, err)
	}
	session, err := peer.NewSession(ctx, sdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []sdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := peer.Prompt(ctx, sdk.PromptRequest{SessionId: session.SessionId, Prompt: []sdk.ContentBlock{sdk.TextBlock("hello")}})
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("prompt: %+v %v", result, err)
	}
	callback.mu.Lock()
	defer callback.mu.Unlock()
	if len(callback.updates) != 1 {
		t.Fatalf("updates: %+v", callback.updates)
	}
	if callback.updates[0].SessionId != session.SessionId {
		t.Fatal("session association lost")
	}
}
