//go:build unix

package runs_test

import (
	"io/fs"
	"syscall"
	"testing"
)

// inodeFromFileInfo is how the lock tests tell "the same file" from "a file
// with the same name". The distinction is the whole of PLAN §1.2.30: a rename
// gives the path a new inode, and an flock follows the inode.
func inodeFromFileInfo(t *testing.T, info fs.FileInfo) uint64 {
	t.Helper()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat information for %s on this platform", info.Name())
	}
	return uint64(st.Ino)
}
