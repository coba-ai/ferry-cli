package fsx_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kurenn/ferry/cli/internal/fsx"
)

func TestWriteFileAtomicSyscallOrder(t *testing.T) {
	dir := t.TempDir()
	f := fsx.NewFault(fsx.OS())
	path := filepath.Join(dir, "record.json")

	if err := fsx.WriteFileAtomic(f, path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	want := []string{fsx.OpCreate, fsx.OpWrite, fsx.OpSync, fsx.OpClose, fsx.OpRename, fsx.OpSyncDir}
	if got := f.Names(); !slices.Equal(got, want) {
		t.Fatalf("operation order:\n got %v\nwant %v", got, want)
	}
}

func TestWriteFileAtomicCreatesTempNotTarget(t *testing.T) {
	dir := t.TempDir()
	f := fsx.NewFault(fsx.OS())
	path := filepath.Join(dir, "record.json")

	if err := fsx.WriteFileAtomic(f, path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	ops := f.Ops()
	if ops[0].Path == path {
		t.Fatalf("create wrote the target path directly: %v", ops[0])
	}
	if ops[0].Perm != 0o600 {
		t.Fatalf("temp created with mode %v, want 0600", ops[0].Perm)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("final mode %v, want 0600", got)
	}
}

// Every step of the atomic write can fail, and none of them may leave a file
// at the target path. This is the property PLAN §5.4 step 9 relies on when it
// says a Begin failure means nothing was sent.
func TestWriteFileAtomicFailureLeavesNothingAtTarget(t *testing.T) {
	for _, op := range []string{fsx.OpCreate, fsx.OpWrite, fsx.OpSync, fsx.OpClose, fsx.OpRename} {
		t.Run(op, func(t *testing.T) {
			dir := t.TempDir()
			f := fsx.NewFault(fsx.OS())
			f.FailAt(op, 1, fsx.ENOSPC)
			path := filepath.Join(dir, "record.json")

			err := fsx.WriteFileAtomic(f, path, []byte(`{"a":1}`), 0o600)
			if err == nil {
				t.Fatalf("expected a failure injected at %s", op)
			}
			if !errors.Is(err, fsx.ENOSPC) {
				t.Fatalf("error %v does not wrap the injected ENOSPC", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("target exists after a failure at %s (stat err %v)", op, err)
			}
			assertNoTempLeft(t, dir)
		})
	}
}

// A directory-fsync failure is the one case where the rename already happened.
// The error is still returned — the caller must not send — and the record is
// present rather than half-written.
func TestWriteFileAtomicSyncDirFailureStillErrorsWithRecordPresent(t *testing.T) {
	dir := t.TempDir()
	f := fsx.NewFault(fsx.OS())
	f.FailAt(fsx.OpSyncDir, 1, fsx.EIO)
	path := filepath.Join(dir, "record.json")

	err := fsx.WriteFileAtomic(f, path, []byte(`{"a":1}`), 0o600)
	if !errors.Is(err, fsx.EIO) {
		t.Fatalf("err = %v, want the injected EIO", err)
	}
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("record should exist after a rename that succeeded: %v", readErr)
	}
	if string(b) != `{"a":1}` {
		t.Fatalf("record contents %q", b)
	}
	assertNoTempLeft(t, dir)
}

func TestWriteFileAtomicReplacesExistingContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	if err := fsx.WriteFileAtomic(fsx.OS(), path, []byte(`{"n":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(fsx.OS(), path, []byte(`{"n":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"n":2}` {
		t.Fatalf("contents %q, want the second write", b)
	}
	assertNoTempLeft(t, dir)
}

// The fault injector must be able to fire, or every test above is vacuous.
func TestFaultInjectorFiresOnTheChosenOccurrence(t *testing.T) {
	dir := t.TempDir()
	f := fsx.NewFault(fsx.OS())
	f.FailAt(fsx.OpRename, 2, fsx.EROFS)

	if err := fsx.WriteFileAtomic(f, filepath.Join(dir, "a.json"), []byte(`1`), 0o600); err != nil {
		t.Fatalf("first write should succeed: %v", err)
	}
	if err := fsx.WriteFileAtomic(f, filepath.Join(dir, "b.json"), []byte(`2`), 0o600); !errors.Is(err, fsx.EROFS) {
		t.Fatalf("second write err = %v, want EROFS", err)
	}
}

// Temp files must never be mistaken for records by a directory sweep: they are
// dotfiles and they do not end in .json.
func assertNoTempLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			t.Fatalf("leftover non-record file %q in %s", e.Name(), dir)
		}
	}
}
