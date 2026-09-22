//go:build goreleaser

package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// AC64's first half, run rather than read: `goreleaser build --snapshot --clean`
// produces the four targets.
//
// # Why this is behind its own tag
//
// goreleaser is not installed on the machines this repository has been developed
// on (PLAN §2 item 24), and the two honest ways to handle that are both bad: a
// test that skips when the binary is absent reads as coverage in the summary, and
// one that fails makes `go test ./...` red on every laptop.
//
// So the tag is the decision. The `cli` CI job installs goreleaser and runs
// `go test -count=1 -tags goreleaser ./e2e/...`, which is asserted by
// `workflow_test.go` in both directions — so the step cannot be dropped without a
// test going red, and the tag cannot become a place code goes to die.
//
// It has been run, against goreleaser 2.18.2 fetched into a scratch directory
// rather than installed, and it found two things reading could not have: that
// `checksums:` is spelled `checksum:` and rejects the whole file otherwise, and
// that the `brews` block AC65 asks for is deprecated to the point where
// `goreleaser check` fails on it (A416). What is still unverified is the half
// that needs a tag and two repositories — the GitHub Release and the push to
// `kurenn/homebrew-tap`. A415 records that.

func goreleaserBinary(t *testing.T) string {
	t.Helper()

	path, err := exec.LookPath("goreleaser")
	if err != nil {
		t.Fatalf("goreleaser is not on PATH (%v). This file is built only with `-tags goreleaser`, "+
			"which is a promise that it is installed; there is no skip here on purpose", err)
	}

	return path
}

// contractDigest is what the release stamps into the binary, computed the same
// way `e2e/run.sh` and the release workflow compute it.
func contractDigest(t *testing.T) string {
	t.Helper()

	out, err := exec.Command("sha256sum", "../contract/openapi.yaml").Output()
	if err != nil {
		t.Fatalf("sha256sum contract/openapi.yaml: %v", err)
	}

	digest, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if len(digest) != 64 {
		t.Fatalf("sha256sum answered %q, which is not a 64-character digest", digest)
	}

	return digest
}

func TestGoreleaserAcceptsTheConfiguration(t *testing.T) {
	bin := goreleaserBinary(t)

	cmd := exec.Command(bin, "check")
	cmd.Dir = ".."

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("`goreleaser check` failed: %v\n%s", err, out)
	}
}

func TestGoreleaserSnapshotBuildsTheFourTargets(t *testing.T) {
	bin := goreleaserBinary(t)

	cmd := exec.Command(bin, "build", "--snapshot", "--clean")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "CONTRACT_SHA256="+contractDigest(t))

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("`goreleaser build --snapshot --clean` failed: %v\n%s", err, out)
	}

	// goreleaser lays a build out as dist/<build id>_<goos>_<goarch>[_<variant>]/<binary>.
	// Found by walking rather than by constructing the paths: the variant
	// suffix depends on goreleaser's version and GOAMD64 settings, and a test
	// that guessed it would fail for a correct build.
	built := map[string]string{}

	root := filepath.Join("..", "dist")

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read dist/: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		path := filepath.Join(root, entry.Name(), "ferry")
		if _, err := os.Stat(path); err != nil {
			continue
		}

		for _, platform := range releasePlatforms {
			goos, goarch, _ := strings.Cut(platform, "/")

			if strings.Contains(entry.Name(), goos) && strings.Contains(entry.Name(), goarch) {
				built[platform] = path
			}
		}
	}

	names := make([]string, 0, len(built))
	for platform := range built {
		names = append(names, platform)
	}

	assertSetsEqual(t, "platforms built into dist/", releasePlatforms, names)

	// And the produced binaries are for the targets they are filed under. The
	// directory name is goreleaser's claim; the header is the measurement.
	for platform, path := range built {
		goos, goarch, _ := strings.Cut(platform, "/")
		assertBinaryTargets(t, path, goos, goarch)
	}
}

// The stamps reached the binary the local platform can run.
func TestTheSnapshotBinaryReportsTheContractDigestItWasBuiltAgainst(t *testing.T) {
	bin := goreleaserBinary(t)
	digest := contractDigest(t)

	cmd := exec.Command(bin, "build", "--snapshot", "--clean", "--single-target")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "CONTRACT_SHA256="+digest)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("`goreleaser build --snapshot --clean --single-target` failed: %v\n%s", err, out)
	}

	var found string

	err := filepath.WalkDir(filepath.Join("..", "dist"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && d.Name() == "ferry" {
			found = path
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk dist/: %v", err)
	}

	if found == "" {
		t.Fatalf("no `ferry` binary under dist/ after a single-target snapshot")
	}

	out, err := exec.Command(found, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("`%s version` failed: %v\n%s", found, err, out)
	}

	if !strings.Contains(string(out), digest) {
		t.Errorf("the snapshot binary does not print the contract digest %s it was built against:\n%s",
			digest, out)
	}

	if !strings.Contains(string(out), "snapshot") {
		t.Errorf("the snapshot binary does not say it is a snapshot. `snapshot.version_template` "+
			"exists so a binary from an untagged tree cannot be read as a release:\n%s", out)
	}
}
