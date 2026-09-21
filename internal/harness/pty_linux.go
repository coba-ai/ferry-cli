//go:build linux

package harness

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPTY allocates a pseudo-terminal and returns the master and slave ends.
//
// This is done with raw ioctls rather than a pty library because PLAN §5.13
// fixes the CLI's dependency set, and §5.13 treats every dependency of a
// money tool as part of its threat model. The cost is that the ioctl numbers
// below are platform-specific, so this file is Linux-only and a port needs a
// sibling.
func openPTY() (master, slave *os.File, err error) {
	// O_NOCTTY: the test process must not acquire this pty as its controlling
	// terminal. It would inherit a terminal it does not control the lifetime
	// of, and a later close would deliver SIGHUP to the test binary itself.
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = m.Close()
		}
	}()

	// TIOCSPTLCK with 0 unlocks the slave; without it the open below fails.
	var unlock int32
	if err := ioctl(m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}

	var n uint32
	if err := ioctl(m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		return nil, nil, fmt.Errorf("get pty number: %w", err)
	}

	name := fmt.Sprintf("/dev/pts/%d", n)
	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", name, err)
	}
	return m, s, nil
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); errno != 0 {
		return errno
	}
	return nil
}
