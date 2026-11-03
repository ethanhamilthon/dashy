//go:build darwin || linux

package osc7501

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// openTTY returns a pseudo-terminal pair. The slave is a real terminal, so the
// terminal checks of Enabled can be exercised on the output and stdin side.
func openTTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	name, err := ptsName(int(master.Fd()))
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	slave, err = os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}
