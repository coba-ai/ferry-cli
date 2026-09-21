//go:build unix

package xdg_test

import "syscall"

// setUmask sets the process umask and returns the previous value. The
// directory-mode assertion needs it because MkdirAll's mode is masked, so a
// developer shell with umask 022 would make a 0700 request land as 0700
// anyway while a 0755 request (mutation M9) would land as 0755 — and the test
// must be able to tell those apart under any umask the caller happens to have.
func setUmask(mask int) int { return syscall.Umask(mask) }
