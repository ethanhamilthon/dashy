package acp

import (
	"golang.org/x/sys/unix"
	"os"
)

// Standard descriptors may use blocking reads that Close cannot interrupt. A
// pollable duplicate lets signal/disconnect cancellation unblock Go's netpoller.
func pollable(file *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}
