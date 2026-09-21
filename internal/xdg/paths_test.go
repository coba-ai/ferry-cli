package xdg_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kurenn/ferry-cli/internal/fsx"
	"github.com/kurenn/ferry-cli/internal/xdg"
)

// AC6: FERRY_HOME > XDG_CONFIG_HOME/XDG_STATE_HOME > ~/.config/ferry,
// ~/.local/state/ferry.
func TestResolutionOrder(t *testing.T) {
	cases := []struct {
		name          string
		env           map[string]string
		config, state string
	}{
		{
			name:   "FERRY_HOME wins over everything",
			env:    map[string]string{"FERRY_HOME": "/srv/ferry", "XDG_CONFIG_HOME": "/xc", "XDG_STATE_HOME": "/xs", "HOME": "/home/u"},
			config: "/srv/ferry",
			state:  "/srv/ferry",
		},
		{
			name:   "XDG wins over HOME",
			env:    map[string]string{"XDG_CONFIG_HOME": "/xc", "XDG_STATE_HOME": "/xs", "HOME": "/home/u"},
			config: "/xc/ferry",
			state:  "/xs/ferry",
		},
		{
			name:   "XDG_CONFIG_HOME alone leaves state on the HOME default",
			env:    map[string]string{"XDG_CONFIG_HOME": "/xc", "HOME": "/home/u"},
			config: "/xc/ferry",
			state:  "/home/u/.local/state/ferry",
		},
		{
			name:   "XDG_STATE_HOME alone leaves config on the HOME default",
			env:    map[string]string{"XDG_STATE_HOME": "/xs", "HOME": "/home/u"},
			config: "/home/u/.config/ferry",
			state:  "/xs/ferry",
		},
		{
			name:   "HOME defaults",
			env:    map[string]string{"HOME": "/home/u"},
			config: "/home/u/.config/ferry",
			state:  "/home/u/.local/state/ferry",
		},
		{
			name:   "an empty variable is an unset variable",
			env:    map[string]string{"FERRY_HOME": "", "XDG_CONFIG_HOME": "", "HOME": "/home/u"},
			config: "/home/u/.config/ferry",
			state:  "/home/u/.local/state/ferry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := xdg.Resolve(xdg.MapEnv(tc.env))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if p.ConfigDir != tc.config {
				t.Errorf("ConfigDir = %q, want %q", p.ConfigDir, tc.config)
			}
			if p.StateDir != tc.state {
				t.Errorf("StateDir = %q, want %q", p.StateDir, tc.state)
			}
		})
	}
}

func TestFilePaths(t *testing.T) {
	p, err := xdg.Resolve(xdg.MapEnv(map[string]string{"FERRY_HOME": "/srv/ferry"}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.CredentialsFile(), "/srv/ferry/credentials.json"; got != want {
		t.Errorf("CredentialsFile = %q, want %q", got, want)
	}
	if got, want := p.RunsDir(), "/srv/ferry/runs"; got != want {
		t.Errorf("RunsDir = %q, want %q", got, want)
	}
}

// AC6: directories are created 0700. Mutation M9 makes them 0755.
func TestEnsureAllCreatesDirectories0700(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "nested", "ferry")
	p, err := xdg.Resolve(xdg.MapEnv(map[string]string{"FERRY_HOME": home}))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureAll(fsx.OS()); err != nil {
		t.Fatalf("EnsureAll: %v", err)
	}

	for _, dir := range []string{p.ConfigDir, p.StateDir, p.RunsDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s created with mode %#o, want 0700", dir, got)
		}
	}
}

// The mode assertion above is only meaningful if the process umask cannot
// relax it. MkdirAll applies the umask, so a umask of 0 must still give 0700 —
// which is what this asserts by creating under a umask that would otherwise
// leave the group and other bits set.
func TestEnsureDirIsNotWidenedByAPermissiveUmask(t *testing.T) {
	old := setUmask(0)
	t.Cleanup(func() { setUmask(old) })

	dir := filepath.Join(t.TempDir(), "runs")
	if err := xdg.EnsureDir(fsx.OS(), dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("with umask 0 the directory is %#o, want 0700", got)
	}
}

func TestEnsureDirRecordsTheModeItAsksFor(t *testing.T) {
	f := fsx.NewFault(fsx.OS())
	dir := filepath.Join(t.TempDir(), "runs")
	if err := xdg.EnsureDir(f, dir); err != nil {
		t.Fatal(err)
	}
	ops := f.Ops()
	if len(ops) != 1 || ops[0].Name != fsx.OpMkdirAll {
		t.Fatalf("operations = %v", ops)
	}
	if ops[0].Perm != 0o700 {
		t.Fatalf("MkdirAll asked for %#o, want 0700", ops[0].Perm)
	}
}

func TestNoHomeIsRefusedRatherThanGuessed(t *testing.T) {
	_, err := xdg.Resolve(xdg.MapEnv(map[string]string{}))
	if !errors.Is(err, xdg.ErrNoHome) {
		t.Fatalf("err = %v, want ErrNoHome", err)
	}
}

func TestRelativePathsAreRefused(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"FERRY_HOME":      {"FERRY_HOME": "relative/ferry"},
		"XDG_CONFIG_HOME": {"XDG_CONFIG_HOME": "xc", "HOME": "/home/u"},
		"XDG_STATE_HOME":  {"XDG_STATE_HOME": "xs", "HOME": "/home/u"},
		"HOME":            {"HOME": "home/u"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := xdg.Resolve(xdg.MapEnv(env)); err == nil {
				t.Fatalf("a relative %s was accepted", name)
			}
		})
	}
}

func TestCheckDirModeFlagsAWideDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	if err := xdg.EnsureDir(fsx.OS(), dir); err != nil {
		t.Fatal(err)
	}
	if err := xdg.CheckDirMode(fsx.OS(), dir); err != nil {
		t.Fatalf("a freshly created directory is reported wide: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := xdg.CheckDirMode(fsx.OS(), dir); !errors.Is(err, xdg.ErrDirTooWide) {
		t.Fatalf("err = %v, want ErrDirTooWide", err)
	}
}
