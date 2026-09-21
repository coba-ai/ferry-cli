// Package fsx is the filesystem seam every durable write in the CLI goes
// through.
//
// It exists for one reason: the money-safety argument in PLAN §2 C1 is a
// claim about syscall order — the idempotency key is written, fsynced and
// renamed into place before the first byte of a money request leaves — and a
// claim about syscall order can only be tested by observing the syscalls and
// by making each one fail. [Fault] does both.
//
// Production code takes [OS]. Tests take [NewFault] wrapped around [OS] (real
// files, so flock and fsync behave), or around a temp dir.
package fsx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Operation names recorded by [Fault]. They are the vocabulary AC10's
// syscall-order assertion is written in.
const (
	OpMkdirAll = "mkdirall"
	OpCreate   = "create"
	OpWrite    = "write"
	OpSync     = "sync"
	OpClose    = "close"
	OpRename   = "rename"
	OpSyncDir  = "syncdir"
	OpRemove   = "remove"
	OpReadFile = "readfile"
	OpStat     = "stat"
	OpReadDir  = "readdir"
)

// File is the subset of *os.File the CLI uses. Fd is part of the interface
// because the run lock is syscall.Flock on a real descriptor (PLAN §1.2.29).
type File interface {
	io.Writer
	io.Closer
	Sync() error
	Name() string
	Fd() uintptr
}

// FS is the filesystem as the CLI sees it.
//
// Create opens name for writing with O_CREATE|O_EXCL|O_WRONLY and the given
// mode; the exclusive flag is what stops a temp file from being adopted from
// another process.
type FS interface {
	MkdirAll(path string, perm fs.FileMode) error
	Create(name string, perm fs.FileMode) (File, error)
	Rename(oldpath, newpath string) error
	SyncDir(dir string) error
	Remove(name string) error
	ReadFile(name string) ([]byte, error)
	Stat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]os.DirEntry, error)
}

type osFS struct{}

// OS returns the real filesystem.
func OS() FS { return osFS{} }

func (osFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

func (osFS) Create(name string, perm fs.FileMode) (File, error) {
	return os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
}

func (osFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// SyncDir fsyncs a directory, which is what makes a rename durable. A rename
// that is not followed by a directory fsync can be lost by a power cut even
// though the file it points at is on the platter.
func (osFS) SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (osFS) Remove(name string) error                   { return os.Remove(name) }
func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }
func (osFS) ReadDir(name string) ([]os.DirEntry, error) { return os.ReadDir(name) }

// WriteFileAtomic is the only durable write in the CLI.
//
// The order is write-temp -> fsync -> close -> rename -> fsync(dir), and it is
// the order AC10 asserts against the recorded operations. A failure at any
// step removes the temp file and leaves nothing at path: a caller that gets an
// error from this function knows the record it was trying to write does not
// exist, which is why PLAN §5.4 step 9 can say "failure -> exit 1, nothing
// sent".
//
// The one window this does not close is a failure of the final directory
// fsync: by then the rename has happened, so path exists but may not survive a
// power cut. The error is still returned, so the caller stops before sending —
// fail-closed in the direction that costs a refusal rather than a transfer.
func WriteFileAtomic(filesystem FS, path string, data []byte, perm fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp"+randomSuffix())

	f, err := filesystem.Create(tmp, perm)
	if err != nil {
		return fmt.Errorf("create temp %s: %w", tmp, err)
	}
	defer func() {
		if err != nil {
			// Best effort: the temp file is named so that no reader of the
			// directory will mistake it for a record.
			_ = filesystem.Remove(tmp)
		}
	}()

	if _, err = f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write temp %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync temp %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close temp %s: %w", tmp, err)
	}
	if err = filesystem.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	if err = filesystem.SyncDir(dir); err != nil {
		// The rename happened. Remove the temp path only; path itself is now a
		// real record and deleting it would be worse than leaving it.
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	return nil
}

// ErrNotFound reports a missing path regardless of the FS implementation.
func ErrNotFound(err error) bool { return errors.Is(err, fs.ErrNotExist) }
