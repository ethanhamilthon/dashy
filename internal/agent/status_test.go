package agent

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStatusEventsDeliveredWithoutHooks(t *testing.T) {
	a := &Agent{
		HistoryPath: filepath.Join(t.TempDir(), "sess-123.jsonl"),
		settings: Settings{
			Provider: "testprov",
			Model:    "testmodel",
			Effort:   "high",
		},
	}
	// a.hooks is nil
	ch := a.StatusEvents()
	if ch == nil {
		t.Fatal("StatusEvents returned nil channel")
	}

	a.Fire("turn_start", "DASHY_TURN_SOURCE", "user", "CUSTOM_VAR", "val", "DASHY_MODE", "headless")

	select {
	case ev := <-ch:
		if ev["DASHY_EVENT"] != "turn_start" {
			t.Fatalf("expected turn_start, got %q", ev["DASHY_EVENT"])
		}
		if ev["DASHY_SESSION_ID"] != "sess-123" {
			t.Fatalf("expected sess-123, got %q", ev["DASHY_SESSION_ID"])
		}
		if ev["DASHY_PROVIDER"] != "testprov" || ev["DASHY_MODEL"] != "testmodel" || ev["DASHY_EFFORT"] != "high" {
			t.Fatalf("unexpected provider/model/effort in event: %v", ev)
		}
		if ev["DASHY_TURN_SOURCE"] != "user" {
			t.Fatalf("expected DASHY_TURN_SOURCE user, got %q", ev["DASHY_TURN_SOURCE"])
		}
		// Non-allowlisted scalar is filtered out early in status bridge
		if _, ok := ev["CUSTOM_VAR"]; ok {
			t.Fatalf("expected CUSTOM_VAR to be filtered out, but found: %q", ev["CUSTOM_VAR"])
		}
		// DASHY_MODE is exported to hooks only, never to status environments.
		if _, ok := ev["DASHY_MODE"]; ok {
			t.Fatalf("expected DASHY_MODE to be filtered out, but found: %q", ev["DASHY_MODE"])
		}
	default:
		t.Fatal("expected status event to be delivered")
	}
}

func TestStatusPayloadFilteringInBridge(t *testing.T) {
	a := &Agent{
		HistoryPath: filepath.Join(t.TempDir(), "sess-payload.jsonl"),
	}
	ch := a.StatusEvents()
	if ch == nil {
		t.Fatal("StatusEvents returned nil channel")
	}

	hugeMessage := strings.Repeat("M", 200*1024)
	hugeCommand := strings.Repeat("C", 1024*1024)
	hugeNotify := strings.Repeat("N", 1024*1024)

	a.Fire("turn_start",
		"DASHY_TURN_SOURCE", "user",
		"DASHY_MESSAGE_FILE", hugeMessage,
		"DASHY_COMMAND_FILE", hugeCommand,
		"DASHY_NOTIFY_FILE", hugeNotify,
	)

	select {
	case ev := <-ch:
		if ev["DASHY_EVENT"] != "turn_start" {
			t.Fatalf("expected turn_start, got %q", ev["DASHY_EVENT"])
		}
		if ev["DASHY_TURN_SOURCE"] != "user" {
			t.Fatalf("expected user, got %q", ev["DASHY_TURN_SOURCE"])
		}
		if _, ok := ev["DASHY_MESSAGE_FILE"]; ok {
			t.Fatal("DASHY_MESSAGE_FILE payload should have been filtered out")
		}
		if _, ok := ev["DASHY_COMMAND_FILE"]; ok {
			t.Fatal("DASHY_COMMAND_FILE payload should have been filtered out")
		}
		if _, ok := ev["DASHY_NOTIFY_FILE"]; ok {
			t.Fatal("DASHY_NOTIFY_FILE payload should have been filtered out")
		}
	default:
		t.Fatal("expected status event to be delivered")
	}
}

func TestStatusEventsChannelNonblocking(t *testing.T) {
	a := &Agent{
		HistoryPath: filepath.Join(t.TempDir(), "s.jsonl"),
	}
	ch := a.StatusEvents()
	// Channel capacity is 64. Send 100 events to ensure nonblocking drop of oldest.
	for i := 0; i < 100; i++ {
		a.Fire("test_event", "DASHY_DURATION_MS", strconv.Itoa(i))
	}
	// Should have 64 elements, not blocked
	if len(ch) != 64 {
		t.Fatalf("expected channel buffer full (64), got %d", len(ch))
	}
	// The newest event should have arrived (DASHY_DURATION_MS=99)
	var last map[string]string
	for len(ch) > 0 {
		last = <-ch
	}
	if last["DASHY_DURATION_MS"] != "99" {
		t.Fatalf("expected newest event DASHY_DURATION_MS 99, got %q", last["DASHY_DURATION_MS"])
	}
}

func TestConcurrentFireAndRequestMetadataNoRace(t *testing.T) {
	a := &Agent{
		HistoryPath: filepath.Join(t.TempDir(), "race.jsonl"),
	}
	a.Start("0.6.0")
	ch := a.StatusEvents()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Goroutine 1: simulating the turn loop / model calls writing lastRequest
	wg.Add(1)
	go func() {
		defer wg.Done()
		var n int64 = 1000
		for {
			select {
			case <-stop:
				return
			default:
				n++
				a.lastRequest = Metadata{
					RequestID:    "r-test",
					ContextInput: &n,
				}
			}
		}
	}()

	// Goroutine 2: simulating UI callbacks / steering firing events concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				a.Fire("steer_queued", "DASHY_MESSAGE_FILE", "some steering command")
				a.Fire("idle")
			}
		}
	}()

	// Goroutine 3: reading status events
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-ch:
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
