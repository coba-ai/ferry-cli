package cli_test

// What .gitignore ignores, and what it must not.
//
// OWNERSHIP's `ignored:` list named `dist/` and `ferry` as gitignored from
// the day of the split, and there was no .gitignore in the repository (A418).
// The list was not describing a state of affairs; it was describing an
// intention nobody had implemented. So `git add -A` after a goreleaser run
// would have committed the release archives, and the reason to notice is that
// OWNERSHIP's own test passes either way — it reads the list, not the
// filesystem.
//
// The patterns are asserted by asking git rather than by reading the file,
// because the property that matters is what git does. In particular the
// anchoring: an unanchored `ferry` matches any path component of that name,
// which includes `cmd/ferry/`, and the package that builds the binary would
// then quietly stop accepting new files. Reading the file could not tell the
// two apart; `git check-ignore` can.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ignored asks git, from the repository root, whether it would ignore a path.
// The path need not exist: check-ignore matches patterns, not inodes, which
// is what lets this ask about a dist/ nobody has built.
func ignored(t *testing.T, path string) bool {
	t.Helper()

	cmd := exec.Command("git", "check-ignore", "-q", "--no-index", "--", path)

	err := cmd.Run()
	if err == nil {
		return true
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false
	}

	// Exit 128 is git refusing the question — not a repository, or a
	// pathspec it will not take. Answering "not ignored" to that would be
	// a green test measuring nothing.
	t.Fatalf("git check-ignore %s: %v", path, err)

	return false
}

func TestTheBuildOutputIsIgnored(t *testing.T) {
	if _, err := os.Stat(".gitignore"); err != nil {
		t.Fatalf("there is no .gitignore, and OWNERSHIP's ignored list says there is: %v", err)
	}

	for _, path := range []string{
		"dist/",
		"dist/ferry_0.1.0_linux_amd64.tar.gz",
		"ferry",
		"ferry-fault",
	} {
		if !ignored(t, path) {
			t.Errorf("%s is not ignored, and OWNERSHIP's ignored list says it is. Every "+
				"entry on that list is a path no unit owns; unignored, it is a path no "+
				"unit owns and git offers to commit.", path)
		}
	}
}

// The other direction, which is the one with a trap in it.
func TestTheSourceThatBuildsTheBinaryIsNotIgnored(t *testing.T) {
	// A new file in the package whose directory is called `ferry`. This is
	// what an unanchored pattern swallows, and it fails open: the file is
	// simply absent from `git status`, so the first symptom is a build that
	// works locally and a CI checkout that does not compile.
	for _, path := range []string{
		filepath.Join("cmd", "ferry", "flags.go"),
		filepath.Join("cmd", "ferry"),
		filepath.Join("internal", "noun", "transfers", "ferry.go"),
	} {
		if ignored(t, path) {
			t.Errorf("%s is ignored. The pattern for the built binary is matching a path "+
				"component rather than the module root, so source under it would never "+
				"appear in git status — which is a missing file nobody is told about "+
				"rather than a failure.", path)
		}
	}

	// And the floor: if check-ignore answered "not ignored" to everything,
	// the loop above would pass over a .gitignore that ignored nothing.
	// TestTheBuildOutputIsIgnored is that floor, so this asserts the two
	// tests cannot both be satisfied by a broken reader.
	if !ignored(t, "dist/") {
		t.Fatal("dist/ is not ignored either, so this test's reader is answering " +
			"'not ignored' to every question and proves nothing")
	}
}

// OWNERSHIP says which paths are ignored; .gitignore says which paths git
// ignores. Nothing had joined the two.
func TestEveryPathOwnershipCallsIgnoredIsIgnored(t *testing.T) {
	b, err := os.ReadFile("OWNERSHIP")
	if err != nil {
		t.Fatalf("reading OWNERSHIP: %v", err)
	}

	// The `ignored:` block's `- path:` entries, read as text rather than
	// through the YAML shape ownership_test.go already parses, so this test
	// keeps working if that shape changes underneath it.
	rest := string(b)

	at := strings.Index(rest, "\nignored:")
	if at < 0 {
		t.Fatal("OWNERSHIP has no ignored: block, and this test is named for it")
	}

	var paths []string

	for _, line := range strings.Split(rest[at:], "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- path:") {
			continue
		}

		paths = append(paths, strings.TrimSpace(strings.TrimPrefix(trimmed, "- path:")))
	}

	if len(paths) == 0 {
		t.Fatal("no ignored paths were parsed out of OWNERSHIP, so this check is vacuous")
	}

	for _, path := range paths {
		if !ignored(t, path) {
			t.Errorf("OWNERSHIP's ignored list names %q and git does not ignore it. Either "+
				"add the pattern to .gitignore or stop claiming it is there: an ignore "+
				"list that describes an intention is how a file quietly stops being "+
				"owned, which is the thing that list exists to prevent.", path)
		}
	}
}
