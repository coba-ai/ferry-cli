// Package version holds the build identity of the binary.
//
// Every value here is overridable at link time with -X, which is how
// goreleaser stamps a release (PLAN §5.15). The defaults are what a
// `go build` with no flags produces, and they say so rather than pretending
// to be a release.
package version

import (
	"runtime"
	"strings"
)

// Set with -ldflags "-X github.com/kurenn/ferry-cli/internal/version.Version=..."
var (
	// Version is the semantic version of the release, without a leading "v".
	Version = "0.0.0-dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// ContractSHA256 is the sha256 of docs/api/openapi.yaml at build time. It
	// travels on the wire in the User-Agent so a server log can say which
	// contract a client was built against (PLAN §5.15; there is no handshake,
	// WD3).
	ContractSHA256 = "unknown"
)

// Info is the whole build identity, for `ferry version` and for the
// ferry_cli block of the JSON document.
type Info struct {
	Version        string `json:"version"`
	Commit         string `json:"commit"`
	GoVersion      string `json:"go_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	ContractSHA256 string `json:"contract_sha256"`
}

// Get returns the build identity.
func Get() Info {
	return Info{
		Version:        Version,
		Commit:         Commit,
		GoVersion:      runtime.Version(),
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		ContractSHA256: ContractSHA256,
	}
}

// ContractSHA8 is the first eight characters of the contract digest, which is
// the form that goes in the User-Agent.
func ContractSHA8() string {
	if len(ContractSHA256) < 8 {
		return ContractSHA256
	}
	return ContractSHA256[:8]
}

// UserAgent is the exact string AC15 pins:
//
//	ferry-cli/<version> (<os>/<arch>; go<ver>) contract/<sha8>
//
// runtime.Version() already carries the "go" prefix, so <ver> is what remains
// after it is trimmed.
func UserAgent() string {
	i := Get()
	return "ferry-cli/" + i.Version +
		" (" + i.OS + "/" + i.Arch + "; go" + strings.TrimPrefix(i.GoVersion, "go") + ")" +
		" contract/" + ContractSHA8()
}
