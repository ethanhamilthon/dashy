//go:build darwin

package osc7501

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ptsName grants and unlocks the slave of a pseudo-terminal master and returns its path.
func ptsName(fd int) (string, error) {
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		return "", err
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		return "", err
	}
	var buf [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", errno
	}
	return strings.TrimRight(string(buf[:]), "\x00"), nil
}
