package agent

import (
	"strings"
	"sync"
)

// maxStatusMetadataValueLen caps the length of individual metadata values
// sent to the status channel, preventing unbounded payloads.
const maxStatusMetadataValueLen = 4096

// IsAllowedStatusMetadataKey reports whether a variable key is an allowed small scalar
// metadata variable for status environments. It rejects all *_FILE payload keys and any
// keys not on the explicit metadata allowlist.
func IsAllowedStatusMetadataKey(key string) bool {
	if strings.HasSuffix(key, "_FILE") {
		return false
	}
	switch key {
	case "DASHY_EVENT",
		"DASHY_SESSION_ID",
		"DASHY_SESSION_SOURCE",
		"DASHY_SESSION_TITLE",
		"DASHY_PREV_SESSION_ID",
		"DASHY_PROVIDER",
		"DASHY_MODEL",
		"DASHY_EFFORT",
		"DASHY_PREV_PROVIDER",
		"DASHY_PREV_MODEL",
		"DASHY_PREV_EFFORT",
		"DASHY_CWD",
		"DASHY_ENDPOINT",
		"DASHY_VERSION",
		"DASHY_STATE",
		"DASHY_CTX_SIZE",
		"DASHY_TURN_SOURCE",
		"DASHY_TURN_STATUS",
		"DASHY_TURN_DURATION_MS",
		"DASHY_DURATION_MS",
		"DASHY_TIMEOUT",
		"DASHY_EXIT_CODE",
		"DASHY_ERROR",
		"DASHY_COMPACT_REASON",
		"DASHY_COMPACT_STATUS",
		"DASHY_IMAGE_SOURCE",
		"DASHY_INPUT_TOKENS",
		"DASHY_OUTPUT_TOKENS",
		"DASHY_ATTEMPT",
		"DASHY_REASON":
		return true
	default:
		return false
	}
}

type statusBridge struct {
	mu sync.Mutex
	ch chan map[string]string
}

var globalStatusMu sync.Mutex

func (a *Agent) getStatus() *statusBridge {
	if a == nil {
		return nil
	}
	globalStatusMu.Lock()
	defer globalStatusMu.Unlock()
	if a.status == nil {
		a.status = &statusBridge{ch: make(chan map[string]string, 64)}
	}
	return a.status
}

// StatusEvents returns a receive-only channel of immutable lifecycle event metadata snapshots.
func (a *Agent) StatusEvents() <-chan map[string]string {
	sb := a.getStatus()
	if sb == nil {
		return nil
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.ch
}

func (a *Agent) emitStatus(event string, vars map[string]string) {
	sb := a.getStatus()
	if sb == nil {
		return
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.ch == nil {
		return
	}
	snapshot := make(map[string]string, len(vars)+4)
	for k, v := range vars {
		if strings.HasSuffix(k, "_FILE") || !IsAllowedStatusMetadataKey(k) {
			continue
		}
		if len(v) > maxStatusMetadataValueLen {
			v = v[:maxStatusMetadataValueLen]
		}
		snapshot[k] = v
	}
	snapshot["DASHY_EVENT"] = event
	if _, ok := snapshot["DASHY_VERSION"]; !ok && a.version != "" {
		snapshot["DASHY_VERSION"] = a.version
	}
	select {
	case sb.ch <- snapshot:
	default:
		select {
		case <-sb.ch:
		default:
		}
		select {
		case sb.ch <- snapshot:
		default:
		}
	}
}
