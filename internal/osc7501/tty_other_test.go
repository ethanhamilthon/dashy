//go:build !darwin && !linux

package osc7501

import (
	"os"
	"testing"
)

// openTTY is unavailable without pseudo-terminal support; the gate test is skipped.
func openTTY(t *testing.T) (master, slave *os.File) {
	t.Skip("no pseudo-terminal on this platform")
	return nil, nil
}
