//go:build !linux

package harness

import (
	"errors"
	"os"
)

// openPTY is unimplemented off Linux.
//
// Run's tty=false path works everywhere; only tty=true needs this. Failing
// loudly is the right answer: a consent test that silently ran without a
// terminal would assert the headless path (AC49) while claiming to cover the
// interactive one (AC48), and would pass for the wrong reason.
func openPTY() (master, slave *os.File, err error) {
	return nil, nil, errors.New("harness: tty=true needs a pty, which is only implemented for linux; " +
		"see internal/harness/pty_linux.go — port it rather than skipping the test")
}
