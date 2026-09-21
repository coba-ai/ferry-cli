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
	if err := control(m, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}

	var n uint32
	if err := control(m, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		return nil, nil, fmt.Errorf("get pty number: %w", err)
	}

	name := fmt.Sprintf("/dev/pts/%d", n)
	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", name, err)
	}
	return m, s, nil
}

// control runs an ioctl against f's descriptor without taking f out of the
// runtime poller.
//
// `os.File.Fd()` is the obvious way to get the descriptor and the wrong one:
// it puts the file into blocking mode and unregisters it, which is documented
// as breaking SetDeadline and, less visibly, breaks Close as well. A Close on
// an unregistered file cannot interrupt a Read already in flight — it marks
// the descriptor closed and waits for the read to finish on its own.
//
// That is not hypothetical here. `drain` sits in a Read on the master for the
// whole of Run, and the one recovery path when the slave will not release
// (A358) is to close the master and expect the read to fail. With `Fd()` that
// recovery silently does nothing and the wait escalates to a hang, which is
// what A406 measured before this changed. SyscallConn keeps the file
// pollable, so Close interrupts the read as intended.
func control(f *os.File, req, arg uintptr) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}

	var inner error

	if err := conn.Control(func(fd uintptr) { inner = ioctl(fd, req, arg) }); err != nil {
		return err
	}

	return inner
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); errno != 0 {
		return errno
	}
	return nil
}
