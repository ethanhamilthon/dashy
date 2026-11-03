//go:build linux

package osc7501

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// ptsName unlocks the slave of a pseudo-terminal master and returns its path.
func ptsName(fd int) (string, error) {
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return "", err
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	return "/dev/pts/" + strconv.Itoa(n), err
}
