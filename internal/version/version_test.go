package version_test

import (
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/kurenn/ferry/cli/internal/version"
)

// AC15 pins the User-Agent shape. The grammar is asserted here rather than
// only in the client so that a link-time stamp that produces a malformed
// header fails in the package that builds it.
func TestUserAgentShape(t *testing.T) {
	ua := version.UserAgent()
	re := regexp.MustCompile(`^ferry-cli/\S+ \([a-z0-9]+/[a-z0-9]+; go\S+\) contract/\S+$`)
	if !re.MatchString(ua) {
		t.Fatalf("User-Agent %q does not match %s", ua, re)
	}
	if strings.Contains(ua, "gogo") {
		t.Fatalf("User-Agent doubles the go prefix: %q", ua)
	}
	if !strings.Contains(ua, "go"+strings.TrimPrefix(runtime.Version(), "go")) {
		t.Fatalf("User-Agent %q does not carry the running Go version", ua)
	}
}

func TestContractSHA8IsEightCharactersWhenAvailable(t *testing.T) {
	orig := version.ContractSHA256
	t.Cleanup(func() { version.ContractSHA256 = orig })

	version.ContractSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := version.ContractSHA8(); got != "01234567" {
		t.Fatalf("ContractSHA8 = %q", got)
	}

	// An unstamped build must not panic; it reports what it has.
	version.ContractSHA256 = "unknown"
	if got := version.ContractSHA8(); got != "unknown" {
		t.Fatalf("ContractSHA8 on an unstamped build = %q", got)
	}
}

func TestGetReportsTheRuntime(t *testing.T) {
	i := version.Get()
	if i.GoVersion != runtime.Version() || i.OS != runtime.GOOS || i.Arch != runtime.GOARCH {
		t.Fatalf("Info does not describe the running binary: %+v", i)
	}
}
