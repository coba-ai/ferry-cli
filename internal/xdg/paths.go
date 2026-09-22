// Package xdg resolves where the CLI keeps its credentials and its run
// ledger.
//
// The resolution order is FERRY_HOME, then the XDG variables, then the
// conventional defaults (AC6). Every directory it creates is 0700, because
// the two things it holds are a bearer token and, for at most one run, a plan
// token (PLAN §5.2, §5.3).
package xdg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/coba-ai/ferry-cli/internal/fsx"
)

// DirMode is the mode every directory the CLI creates is given.
const DirMode fs.FileMode = 0o700

// Env looks a variable up. It is a parameter rather than a call to os.Getenv
// so a test can state the whole environment it means to resolve against
// instead of mutating the process (and inheriting whatever the developer's
// shell exported).
type Env func(key string) string

// OSEnv reads the process environment.
func OSEnv(key string) string { return os.Getenv(key) }

// MapEnv reads a fixed map. A key that is absent reads as empty, exactly as an
// unset variable does.
func MapEnv(m map[string]string) Env {
	return func(key string) string { return m[key] }
}

// ErrNoHome reports that neither FERRY_HOME, the relevant XDG variable, nor a
// home directory could be determined. It is a refusal rather than a guess: a
// CLI that invents a location for a credentials file writes a bearer token
// somewhere its owner does not know about.
var ErrNoHome = errors.New("xdg: cannot determine a home directory; set FERRY_HOME")

// Paths is where this invocation's state lives.
type Paths struct {
	// ConfigDir holds credentials.json.
	ConfigDir string
	// StateDir holds runs/.
	StateDir string
	// Home is the FERRY_HOME that produced both, or "" when the XDG or
	// default paths were used.
	Home string
}

// Resolve computes the paths from an environment.
//
// FERRY_HOME wins outright and collapses config and state into one directory,
// which is the shape PLAN §5.2 and §5.3 write ($FERRY_HOME/credentials.json,
// $FERRY_HOME/runs/<id>.json) and the shape every test and the e2e job set.
// Otherwise config comes from XDG_CONFIG_HOME or ~/.config and state from
// XDG_STATE_HOME or ~/.local/state, each with a "ferry" component.
//
// A relative path in any of the three variables is refused: the CLI changes no
// directory, but a relative state directory would put a run ledger wherever
// the caller happened to be standing, and a resume from another directory
// would not find it.
func Resolve(env Env) (Paths, error) {
	if env == nil {
		env = OSEnv
	}

	if home := env("FERRY_HOME"); home != "" {
		if !filepath.IsAbs(home) {
			return Paths{}, fmt.Errorf("xdg: FERRY_HOME must be an absolute path, got %q", home)
		}
		clean := filepath.Clean(home)
		return Paths{ConfigDir: clean, StateDir: clean, Home: clean}, nil
	}

	config, err := dirFrom(env, "XDG_CONFIG_HOME", filepath.Join(".config"))
	if err != nil {
		return Paths{}, err
	}
	state, err := dirFrom(env, "XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return Paths{}, err
	}
	return Paths{ConfigDir: config, StateDir: state}, nil
}

func dirFrom(env Env, key, fallbackRel string) (string, error) {
	if v := env(key); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("xdg: %s must be an absolute path, got %q", key, v)
		}
		return filepath.Join(filepath.Clean(v), "ferry"), nil
	}
	home := env("HOME")
	if home == "" {
		return "", ErrNoHome
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("xdg: HOME must be an absolute path, got %q", home)
	}
	return filepath.Join(filepath.Clean(home), fallbackRel, "ferry"), nil
}

// CredentialsFile is the path of the credentials store.
func (p Paths) CredentialsFile() string { return filepath.Join(p.ConfigDir, "credentials.json") }

// RunsDir is the directory the run ledger lives in.
func (p Paths) RunsDir() string { return filepath.Join(p.StateDir, "runs") }

// EnsureDir creates dir and its parents at 0700.
//
// It does not widen or narrow a directory that already exists: repairing a
// directory the caller made world-readable is a decision for the caller, and a
// silent chmod would hide it. Use [CheckDirMode] to find out.
func EnsureDir(filesystem fsx.FS, dir string) error {
	if filesystem == nil {
		filesystem = fsx.OS()
	}
	if err := filesystem.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("xdg: create %s: %w", dir, err)
	}
	return nil
}

// EnsureAll creates the config directory, the state directory and the runs
// directory.
func (p Paths) EnsureAll(filesystem fsx.FS) error {
	for _, d := range []string{p.ConfigDir, p.StateDir, p.RunsDir()} {
		if err := EnsureDir(filesystem, d); err != nil {
			return err
		}
	}
	return nil
}

// ErrDirTooWide reports a state directory readable by somebody other than its
// owner.
var ErrDirTooWide = errors.New("xdg: directory permissions are wider than 0700")

// CheckDirMode reports ErrDirTooWide when dir grants any group or other bit.
//
// Nothing in U1 calls this on the read path — AC6 only requires that what the
// CLI creates is 0700, and refusing to read a pre-existing 0755 directory
// would lock a caller out of their own ledger with no remedy. It is exported
// so a later unit can warn.
func CheckDirMode(filesystem fsx.FS, dir string) error {
	if filesystem == nil {
		filesystem = fsx.OS()
	}
	info, err := filesystem.Stat(dir)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s is %#o", ErrDirTooWide, dir, perm)
	}
	return nil
}
