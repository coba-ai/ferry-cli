package e2e_test

import (
	"debug/elf"
	"debug/macho"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// AC64's second half — `ferry version` prints version, commit, Go version and
// the `openapi.yaml` sha256 — and the part of its first half that does not need
// goreleaser: the four targets compile with CGO_ENABLED=0.
//
// These run in the default suite, with no tag. They are the coverage AC64 has on
// a machine where goreleaser is not installed, which at the time of writing is
// every machine this repository has been built on (PLAN §2 item 24). What they do
// **not** check is goreleaser's own behaviour — that it reads this config, honours
// the stamps and produces the archives. `release_test.go` does, behind the
// `goreleaser` tag, and A415 records that it has never been run.

// The values stamped into the binary under test. Chosen to be unmistakable: a
// test that asserted `0.0.0-dev` would pass against an unstamped build, which is
// exactly what mutation M61 produces.
const (
	stampedVersion  = "9.9.9-e2e"
	stampedCommit   = "c0ffee0000000000000000000000000000000000"
	stampedContract = "1111111111111111111111111111111111111111111111111111111111111111"
)

// buildFerry compiles cmd/ferry for one target, with the release build's flags.
//
// `..` because this package is `e2e/` and the module root is above it.
func buildFerry(t *testing.T, goos, goarch string, stamp bool) string {
	t.Helper()

	name := "ferry"
	if goos == "windows" {
		name += ".exe"
	}

	out := filepath.Join(t.TempDir(), name)

	args := []string{"build", "-trimpath", "-o", out}

	if stamp {
		args = append(args, "-ldflags", strings.Join([]string{
			"-s", "-w",
			"-X github.com/kurenn/ferry-cli/internal/version.Version=" + stampedVersion,
			"-X github.com/kurenn/ferry-cli/internal/version.Commit=" + stampedCommit,
			"-X github.com/kurenn/ferry-cli/internal/version.ContractSHA256=" + stampedContract,
		}, " "))
	}

	cmd := exec.Command("go", append(args, "./cmd/ferry")...)
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)

	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build for %s/%s failed: %v\n%s", goos, goarch, err, combined)
	}

	return out
}

func TestFerryVersionPrintsEveryPieceOfBuildIdentity(t *testing.T) {
	bin := buildFerry(t, runtime.GOOS, runtime.GOARCH, true)

	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("`ferry version` failed: %v\n%s", err, out)
	}

	text := string(out)

	// AC64 names four things. Each is asserted against the value that was
	// *stamped in*, not against a pattern: a pattern would pass for a binary
	// that printed its own defaults, and the defaults are what a build with no
	// -ldflags produces.
	for what, want := range map[string]string{
		"the version":         stampedVersion,
		"the commit":          stampedCommit,
		"the contract sha256": stampedContract,
		"the Go version":      runtime.Version(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("`ferry version` does not print %s (%q):\n%s", what, want, text)
		}
	}
}

// The control for the test above.
//
// Without it, `TestFerryVersionPrintsEveryPieceOfBuildIdentity` could be passing
// because the strings happen to appear for some other reason. An unstamped build
// must print the placeholders the version package declares and none of the
// stamped values — which is the same measurement as mutation M61, "drop the -X",
// taken from the other side.
func TestAnUnstampedBuildSaysSoRatherThanClaimingAVersion(t *testing.T) {
	bin := buildFerry(t, runtime.GOOS, runtime.GOARCH, false)

	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("`ferry version` failed: %v\n%s", err, out)
	}

	text := string(out)

	for _, stamped := range []string{stampedVersion, stampedCommit, stampedContract} {
		if strings.Contains(text, stamped) {
			t.Errorf("an unstamped build printed %q, so the stamped test proves nothing:\n%s",
				stamped, text)
		}
	}

	// And it must say what it is rather than inventing a release.
	for _, placeholder := range []string{"0.0.0-dev", "unknown"} {
		if !strings.Contains(text, placeholder) {
			t.Errorf("an unstamped build does not print %q; internal/version's defaults are what "+
				"make a local build distinguishable from a release:\n%s", placeholder, text)
		}
	}
}

// AC64's four targets, compiled.
//
// The binary's own header is read back, because "go build exited 0" says nothing
// about which platform it built for: a GOOS that the toolchain quietly ignored
// would produce a host binary and a green test.
func TestTheFourReleaseTargetsCompileWithoutCgo(t *testing.T) {
	for _, platform := range releasePlatforms {
		t.Run(platform, func(t *testing.T) {
			goos, goarch, ok := strings.Cut(platform, "/")
			if !ok {
				t.Fatalf("%q is not a goos/goarch pair", platform)
			}

			assertBinaryTargets(t, buildFerry(t, goos, goarch, true), goos, goarch)
		})
	}
}

// assertBinaryTargets reads the executable header and holds it to the target.
func assertBinaryTargets(t *testing.T, path, goos, goarch string) {
	t.Helper()

	switch goos {
	case "linux":
		f, err := elf.Open(path)
		if err != nil {
			t.Fatalf("%s is not an ELF binary, so GOOS=linux did not take effect: %v", path, err)
		}

		defer f.Close()

		want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[goarch]
		if f.Machine != want {
			t.Errorf("%s targets %v, want %v", path, f.Machine, want)
		}

		// CGO_ENABLED=0 in one observable form: a pure-Go binary is statically
		// linked and names no dynamic loader. A cgo build has a PT_INTERP
		// segment pointing at ld.so.
		for _, prog := range f.Progs {
			if prog.Type == elf.PT_INTERP {
				t.Errorf("%s has a PT_INTERP segment, so it is dynamically linked and CGO_ENABLED=0 "+
					"did not take effect", path)
			}
		}

	case "darwin":
		f, err := macho.Open(path)
		if err != nil {
			t.Fatalf("%s is not a Mach-O binary, so GOOS=darwin did not take effect: %v", path, err)
		}

		defer f.Close()

		want := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[goarch]
		if f.Cpu != want {
			t.Errorf("%s targets %v, want %v", path, f.Cpu, want)
		}

		// No PT_INTERP equivalent is asserted here. A Mach-O binary always
		// links libSystem, cgo or not, so "names a dynamic library" would fail
		// for a correct build; and cross-compiling from Linux forces cgo off
		// regardless of the variable, so there is nothing this check could
		// distinguish. Said rather than left as an asymmetry a reader has to
		// explain to themselves.

	default:
		t.Fatalf("no header check is written for GOOS=%s", goos)
	}
}
