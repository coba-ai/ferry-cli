package e2e_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// AC64's declarative half: what `.goreleaser.yaml` says it will build.
//
// These pins are not a substitute for running goreleaser — `release_test.go`
// does that, behind the `goreleaser` tag, because goreleaser is not installed on
// every machine (PLAN §2 item 24). They exist because the two things AC64 names
// about the build, four targets and `CGO_ENABLED=0`, are *decided* in this file,
// and a config that stopped saying so would produce a green `goreleaser build`
// of the wrong thing.
//
// Every comparison below is set equality in both directions with its own floor.

const goreleaserFile = "../.goreleaser.yaml"

// The platforms PLAN §5.13 fixes.
var releasePlatforms = []string{
	"darwin/amd64",
	"darwin/arm64",
	"linux/amd64",
	"linux/arm64",
}

// The version package's variables goreleaser must stamp. `ferry version` prints
// all three, and the User-Agent carries two of them.
var stampedVariables = []string{
	"github.com/coba-ai/ferry-cli/internal/version.Version",
	"github.com/coba-ai/ferry-cli/internal/version.Commit",
	"github.com/coba-ai/ferry-cli/internal/version.ContractSHA256",
}

// The other two files that spell the module path out in an `-X` stamp, and
// go.mod, which is the one that decides it.
const (
	goModFile    = "../go.mod"
	e2eRunScript = "run.sh"
)

var moduleLine = regexp.MustCompile(`(?m)^module\s+(\S+)$`)

func declaredModulePath(t *testing.T) string {
	t.Helper()

	b, err := os.ReadFile(goModFile)
	if err != nil {
		t.Fatalf("read %s: %v", goModFile, err)
	}

	m := moduleLine.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("%s declares no `module` line, so there is nothing to hold the stamps to", goModFile)
	}

	return m[1]
}

// A435: the module path is written in four places and the linker forgives
// three of them.
//
// `go.mod` decides it. `.goreleaser.yaml` and `e2e/run.sh` repeat it inside
// `-ldflags -X <path>.Version=…`, and `-X` naming a symbol that does not exist
// is not an error: measured against go1.24.13, `go build -ldflags "-X
// github.com/wrong/path/internal/version.Version=9.9.9"` exits 0 and the binary
// prints `ferry 0.0.0-dev` / `contract unknown`. So a rename that updates
// `go.mod` and every import — which it must, or nothing compiles — and misses
// these two produces a green release of a binary that cannot say which contract
// it was built against. That is the exact outcome
// `TestTheContractDigestIsStampedFromTheEnvironmentWithNoDefault` exists to
// prevent, reached by a route it does not watch: it pins the *template*, and a
// correct template under a stale import path stamps nothing.
//
// `e2e/version_test.go` writes the path a third time and needs no pin here,
// because it asserts the stamped values come back out of `ferry version` — a
// stale path there fails on its own. Nothing reads the version of the binary
// `e2e/run.sh` builds, which is why that one is listed.
//
// Held equal to go.mod rather than to a literal: the literal pin is
// `stampedVariables` above, which this also checks, so "the module was renamed
// and a copy was missed" and "the stamps name the wrong variables" stay two
// different failures.
func TestEveryVersionStampNamesTheModulePathGoModDeclares(t *testing.T) {
	module := declaredModulePath(t)

	// The floor under the literal pin: `stampedVariables` is what
	// `TestTheReleaseStampsExactlyTheThreeVersionVariables` compares the
	// config against, so a rename that updated the config and this file
	// together but left go.mod behind would otherwise agree with itself.
	for _, name := range stampedVariables {
		if !strings.HasPrefix(name, module+"/") {
			t.Errorf("the expected stamp %q is not under the module %s declares (%s). "+
				"Either go.mod was renamed and this list was not, or the reverse",
				name, goModFile, module)
		}
	}

	stamps := stampsIn(loadGoreleaser(t).Builds[0].Ldflags)
	if len(stamps) == 0 {
		t.Fatalf("no -X stamps were read out of %s, so this check would be vacuous", goreleaserFile)
	}

	for name := range stamps {
		if !strings.HasPrefix(name, module+"/") {
			t.Errorf("%s stamps %q, which is not under the module %s declares (%s). The linker "+
				"ignores an -X naming a symbol that is not there, so this builds, releases and "+
				"ships a binary printing `contract unknown`",
				goreleaserFile, name, goModFile, module)
		}
	}

	script, err := os.ReadFile(e2eRunScript)
	if err != nil {
		t.Fatalf("read e2e/%s: %v", e2eRunScript, err)
	}

	found := 0

	for _, part := range splitLdflag(strings.ReplaceAll(string(script), "\n", " ")) {
		m := ldflagX.FindStringSubmatch(strings.Trim(part, `"`))
		if m == nil {
			continue
		}

		found++

		if name := strings.Trim(m[1], `"`); !strings.HasPrefix(name, module+"/") {
			t.Errorf("e2e/%s stamps %q, which is not under the module %s declares (%s)",
				e2eRunScript, name, goModFile, module)
		}
	}

	// The floor: the reader above is a regexp over a shell script, and a
	// rewrite of how `stamps=(…)` is spelled would make every check in this
	// half pass by matching nothing.
	if found != len(stampedVariables) {
		t.Errorf("read %d -X stamps out of e2e/%s, want %d; the suite builds the binary every "+
			"other e2e test drives, and nothing reads its version",
			found, e2eRunScript, len(stampedVariables))
	}

	// The cask's homepage is the fourth spelling of the owner, and the only
	// one a user sees. Nothing breaks when it is stale — `brew info ferry`
	// just links to a 404 — which is why it needs holding here rather than
	// noticing later.
	casks := loadGoreleaser(t).HomebrewCasks
	if len(casks) != 1 {
		t.Fatalf("%s declares %d homebrew_casks blocks, want 1", goreleaserFile, len(casks))
	}

	if cask := casks[0]; cask.Homepage != "https://"+module {
		t.Errorf("the cask's homepage is %q, want %q — the module %s declares",
			cask.Homepage, "https://"+module, goModFile)
	}
}

type goreleaserConfig struct {
	Before struct {
		Hooks []string `yaml:"hooks"`
	} `yaml:"before"`

	Builds []struct {
		ID      string   `yaml:"id"`
		Main    string   `yaml:"main"`
		Binary  string   `yaml:"binary"`
		Env     []string `yaml:"env"`
		Flags   []string `yaml:"flags"`
		Ldflags []string `yaml:"ldflags"`
		Goos    []string `yaml:"goos"`
		Goarch  []string `yaml:"goarch"`
	} `yaml:"builds"`

	// `homebrew_casks` and not `brews`: A416. `brews` is deprecated and
	// `goreleaser check` fails on it, which `release_test.go` measured.
	HomebrewCasks []struct {
		Name       string   `yaml:"name"`
		Homepage   string   `yaml:"homepage"`
		Binaries   []string `yaml:"binaries"`
		Repository struct {
			Owner string `yaml:"owner"`
			Name  string `yaml:"name"`
			Token string `yaml:"token"`
		} `yaml:"repository"`
		Hooks struct {
			Post struct {
				Install string `yaml:"install"`
			} `yaml:"post"`
		} `yaml:"hooks"`
	} `yaml:"homebrew_casks"`

	// Read only so it can be asserted *absent*. A rewrite from the plan's own
	// words would put `brews` back, and the failure is a release that has
	// already published a GitHub Release before goreleaser refuses the config.
	Brews []any `yaml:"brews"`
}

func loadGoreleaser(t *testing.T) goreleaserConfig {
	t.Helper()

	b, err := os.ReadFile(goreleaserFile)
	if err != nil {
		t.Fatalf("read %s: %v (AC64 is about this file; without it there is nothing to check)",
			goreleaserFile, err)
	}

	var cfg goreleaserConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse %s: %v", goreleaserFile, err)
	}

	if len(cfg.Builds) != 1 {
		t.Fatalf("%s declares %d builds; every check below reads the first and would be about "+
			"the wrong one", goreleaserFile, len(cfg.Builds))
	}

	return cfg
}

// The floor for the platform comparison, as its own example.
//
// `goos` × `goarch` is a product, and a config with an empty `goarch` would make
// the product empty — at which point "the declared platforms are exactly the
// four" would compare the empty set with itself only because
// `assertSetsEqual` refuses an empty *expectation*, not an empty actual. This
// asserts the parse found both axes, so "the reader broke" and "the platform set
// changed" fail separately.
func TestTheGoreleaserPlatformReaderFindsBothAxes(t *testing.T) {
	build := loadGoreleaser(t).Builds[0]

	if len(build.Goos) == 0 {
		t.Errorf("%s declares no goos for build %q", goreleaserFile, build.ID)
	}

	if len(build.Goarch) == 0 {
		t.Errorf("%s declares no goarch for build %q", goreleaserFile, build.ID)
	}
}

func TestTheReleaseBuildsExactlyTheFourDeclaredPlatforms(t *testing.T) {
	build := loadGoreleaser(t).Builds[0]

	var declared []string

	for _, goos := range build.Goos {
		for _, goarch := range build.Goarch {
			declared = append(declared, goos+"/"+goarch)
		}
	}

	assertSetsEqual(t, "platforms in "+goreleaserFile, releasePlatforms, declared)
}

func TestTheReleaseBuildDisablesCgo(t *testing.T) {
	build := loadGoreleaser(t).Builds[0]

	if !toSet(build.Env)["CGO_ENABLED=0"] {
		t.Errorf("build %q does not set CGO_ENABLED=0 (env: %v). A cgo binary is bound to the "+
			"glibc of the runner that built it, and these are installed elsewhere",
			build.ID, build.Env)
	}

	if !toSet(build.Flags)["-trimpath"] {
		t.Errorf("build %q does not pass -trimpath (flags: %v); the developer's home directory "+
			"is in the binary", build.ID, build.Flags)
	}
}

// ldflagX matches one `-X <import path>.<name>=<value>` stamp.
var ldflagX = regexp.MustCompile(`^-X\s+(\S+?)=(.*)$`)

// The floor for the stamp comparison, as its own example.
func TestTheLdflagReaderFindsStamps(t *testing.T) {
	build := loadGoreleaser(t).Builds[0]

	if len(stampsIn(build.Ldflags)) == 0 {
		t.Fatalf("no -X stamps were read out of %v; every comparison over them would be vacuous",
			build.Ldflags)
	}
}

func TestTheReleaseStampsExactlyTheThreeVersionVariables(t *testing.T) {
	build := loadGoreleaser(t).Builds[0]

	stamps := stampsIn(build.Ldflags)

	names := make([]string, 0, len(stamps))
	for name := range stamps {
		names = append(names, name)
	}

	assertSetsEqual(t, "-X stamps in "+goreleaserFile, stampedVariables, names)
}

// The contract digest must come from the environment and must have no default.
//
// A default is the failure worth naming: `ContractSHA256` defaults to "unknown"
// in the version package, so a release that forgot to compute the digest would
// build, install, and print `contract unknown` — a binary that cannot say which
// contract it was built against, which is the one thing the stamp exists for.
// goreleaser fails a template naming an unset variable, so referencing it bare
// turns that mistake into a red release.
func TestTheContractDigestIsStampedFromTheEnvironmentWithNoDefault(t *testing.T) {
	build := loadGoreleaser(t).Builds[0]

	stamps := stampsIn(build.Ldflags)

	value, ok := stamps["github.com/coba-ai/ferry-cli/internal/version.ContractSHA256"]
	if !ok {
		t.Fatalf("ContractSHA256 is not stamped at all (ldflags: %v)", build.Ldflags)
	}

	if value != "{{ .Env.CONTRACT_SHA256 }}" {
		t.Errorf("ContractSHA256 is stamped as %q. It must be exactly `{{ .Env.CONTRACT_SHA256 }}`: "+
			"a template with a default, or an `if index .Env` guard, lets a release ship saying "+
			"`contract unknown`", value)
	}
}

// A313: go.mod and go.sum are frozen, and `go mod tidy` reclassifies pflag.
// goreleaser's own scaffold puts tidy in `before.hooks`, so this is the pin that
// stops it coming back with a config regeneration.
func TestNoReleaseHookRunsGoModTidy(t *testing.T) {
	cfg := loadGoreleaser(t)

	if len(cfg.Before.Hooks) == 0 {
		t.Fatalf("%s declares no before hooks; this check would be vacuous, and `go mod download` "+
			"is expected to be one of them", goreleaserFile)
	}

	for _, hook := range cfg.Before.Hooks {
		if strings.Contains(hook, "mod tidy") {
			t.Errorf("before hook %q runs `go mod tidy`. PLAN §4.3.4 and A313 freeze go.mod and "+
				"go.sum after U1, and tidy reclassifies pflag", hook)
		}
	}
}

// AC65's publish clause, as A416 amends it: a cask rather than a formula.
func TestTheCaskGoesToTheDeclaredTap(t *testing.T) {
	cfg := loadGoreleaser(t)

	if len(cfg.Brews) != 0 {
		t.Errorf("%s still declares a `brews` block. It is deprecated, `goreleaser check` fails on "+
			"it, and a release fails *after* publishing the GitHub Release — use `homebrew_casks` "+
			"(A416)", goreleaserFile)
	}

	if len(cfg.HomebrewCasks) != 1 {
		t.Fatalf("%s declares %d homebrew_casks blocks, want 1 (AC65, A416)",
			goreleaserFile, len(cfg.HomebrewCasks))
	}

	cask := cfg.HomebrewCasks[0]

	if cask.Repository.Owner != "coba-ai" || cask.Repository.Name != "homebrew-tap" {
		t.Errorf("the cask goes to %s/%s, want coba-ai/homebrew-tap (AC65)",
			cask.Repository.Owner, cask.Repository.Name)
	}

	// AC65 says `ferry.rb`; the name decides the file and `brew install
	// coba-ai/tap/ferry`. The directory is not asserted because casks have only
	// one valid home and goreleaser defaults to it — pinning `Casks` here would
	// be pinning a default the tool owns.
	if cask.Name != "ferry" {
		t.Errorf("the cask is named %q, want ferry (AC65: ferry.rb, `brew install "+
			"coba-ai/tap/ferry`)", cask.Name)
	}

	assertSetsEqual(t, "binaries the cask installs", []string{"ferry"}, cask.Binaries)

	// The token is what an operator has to provide; naming it in the config is
	// what makes the missing-secret failure say which secret.
	if !strings.Contains(cask.Repository.Token, "HOMEBREW_TAP_GITHUB_TOKEN") {
		t.Errorf("the tap push reads token %q; PLAN §5.15 names HOMEBREW_TAP_GITHUB_TOKEN",
			cask.Repository.Token)
	}

	// Without this the cask installs and every `ferry` invocation is refused by
	// Gatekeeper, because the binary is neither signed nor notarised (§10.1
	// row 7). A cask that installs a binary macOS will not run is not a
	// working install, so it is pinned rather than left as a comment.
	if !strings.Contains(cask.Hooks.Post.Install, "com.apple.quarantine") {
		t.Errorf("the cask has no post-install hook clearing com.apple.quarantine. The binary is "+
			"unsigned, so macOS refuses it on first run:\n%q", cask.Hooks.Post.Install)
	}
}

func stampsIn(ldflags []string) map[string]string {
	stamps := map[string]string{}

	for _, flag := range ldflags {
		// One ldflags entry may hold several flags, as `-s -w` does.
		for _, part := range splitLdflag(flag) {
			if m := ldflagX.FindStringSubmatch(part); m != nil {
				stamps[m[1]] = m[2]
			}
		}
	}

	return stamps
}

// splitLdflag breaks an ldflags entry into the flags it carries, keeping `-X
// name=value` together even though the value may hold spaces (it does: the
// contract template is `{{ .Env.CONTRACT_SHA256 }}`).
func splitLdflag(entry string) []string {
	fields := strings.Fields(entry)

	var out []string

	for i := 0; i < len(fields); i++ {
		if fields[i] == "-X" && i+1 < len(fields) {
			// Everything up to the next flag belongs to this -X.
			value := []string{fields[i+1]}

			for j := i + 2; j < len(fields) && !strings.HasPrefix(fields[j], "-"); j++ {
				value = append(value, fields[j])
				i = j
			}

			out = append(out, "-X "+strings.Join(value, " "))
			i++

			continue
		}

		out = append(out, fields[i])
	}

	return out
}
