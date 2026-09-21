package fsx

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"sync"
)

func randomSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on any platform this ships to; a temp
		// filename is not a security boundary, and O_EXCL is what makes the
		// create safe.
		return "0"
	}
	return hex.EncodeToString(b[:])
}

// Op is one filesystem operation as it was attempted.
type Op struct {
	Name string      // one of the Op* constants
	Path string      // the path the operation names
	Perm fs.FileMode // the mode requested, for Create and MkdirAll
}

func (o Op) String() string { return o.Name + " " + o.Path }

// Fault wraps an FS, records every operation, and can fail a chosen one.
//
// It is what AC10 is proven against: the recording answers "in what order",
// and the injection answers "and what is on disk if this one fails".
//
// A Fault is safe for concurrent use.
type Fault struct {
	base FS

	mu    sync.Mutex
	ops   []Op
	fail  map[string]*failure
	count map[string]int
}

type failure struct {
	nth int // 1-based occurrence of the operation name to fail
	err error
}

// NewFault wraps base.
func NewFault(base FS) *Fault {
	return &Fault{base: base, fail: map[string]*failure{}, count: map[string]int{}}
}

// FailAt makes the nth (1-based) occurrence of the named operation return err.
// Only one injection per operation name is held; the last call wins.
func (f *Fault) FailAt(name string, nth int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[name] = &failure{nth: nth, err: err}
}

// Ops returns the operations recorded so far, in order.
func (f *Fault) Ops() []Op {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Op, len(f.ops))
	copy(out, f.ops)
	return out
}

// Names returns just the operation names recorded so far, in order. This is
// the form AC10's order assertion reads.
func (f *Fault) Names() []string {
	ops := f.Ops()
	out := make([]string, len(ops))
	for i, o := range ops {
		out[i] = o.Name
	}
	return out
}

// Reset clears the recording. Injections are left in place.
func (f *Fault) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = nil
	f.count = map[string]int{}
}

// record logs the operation and reports the injected error, if this occurrence
// is the one selected.
func (f *Fault) record(name, path string, perm fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, Op{Name: name, Path: path, Perm: perm})
	f.count[name]++
	if fl, ok := f.fail[name]; ok && f.count[name] == fl.nth {
		return fl.err
	}
	return nil
}

func (f *Fault) MkdirAll(path string, perm fs.FileMode) error {
	if err := f.record(OpMkdirAll, path, perm); err != nil {
		return err
	}
	return f.base.MkdirAll(path, perm)
}

func (f *Fault) Create(name string, perm fs.FileMode) (File, error) {
	if err := f.record(OpCreate, name, perm); err != nil {
		return nil, err
	}
	h, err := f.base.Create(name, perm)
	if err != nil {
		return nil, err
	}
	return &faultFile{f: f, File: h}, nil
}

func (f *Fault) Rename(oldpath, newpath string) error {
	if err := f.record(OpRename, newpath, 0); err != nil {
		return err
	}
	return f.base.Rename(oldpath, newpath)
}

func (f *Fault) SyncDir(dir string) error {
	if err := f.record(OpSyncDir, dir, 0); err != nil {
		return err
	}
	return f.base.SyncDir(dir)
}

func (f *Fault) Remove(name string) error {
	if err := f.record(OpRemove, name, 0); err != nil {
		return err
	}
	return f.base.Remove(name)
}

func (f *Fault) ReadFile(name string) ([]byte, error) {
	if err := f.record(OpReadFile, name, 0); err != nil {
		return nil, err
	}
	return f.base.ReadFile(name)
}

func (f *Fault) Stat(name string) (fs.FileInfo, error) {
	if err := f.record(OpStat, name, 0); err != nil {
		return nil, err
	}
	return f.base.Stat(name)
}

func (f *Fault) ReadDir(name string) ([]os.DirEntry, error) {
	if err := f.record(OpReadDir, name, 0); err != nil {
		return nil, err
	}
	return f.base.ReadDir(name)
}

type faultFile struct {
	f *Fault
	File
}

func (x *faultFile) Write(p []byte) (int, error) {
	if err := x.f.record(OpWrite, x.File.Name(), 0); err != nil {
		return 0, err
	}
	return x.File.Write(p)
}

func (x *faultFile) Sync() error {
	if err := x.f.record(OpSync, x.File.Name(), 0); err != nil {
		// The descriptor is still open and the caller will close it. An
		// injected fsync failure models EIO, which on Linux may also have
		// dropped the dirty pages; the caller must not treat the data as
		// written, which is what returning the error enforces.
		return err
	}
	return x.File.Sync()
}

func (x *faultFile) Close() error {
	if err := x.f.record(OpClose, x.File.Name(), 0); err != nil {
		_ = x.File.Close()
		return err
	}
	return x.File.Close()
}

// ENOSPC and EROFS are the two failures the adversarial pass in PLAN §7.1 A20
// names. They are exported so a test reads as the scenario it is.
var (
	ENOSPC = fmt.Errorf("no space left on device")
	EROFS  = fmt.Errorf("read-only file system")
	EIO    = fmt.Errorf("input/output error")
)
