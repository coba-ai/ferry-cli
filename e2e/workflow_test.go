package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// AC60 and AC65 — what the workflows do, pinned.
//
// PLAN §3.7 names `spec/ci/workflow_spec.rb` as the spec for both. That file
// does not exist and never did: it was to live in the API repository, where the
// `cli` job used to be, and A400 moved the job here. So the pins are here, in
// Go, in the repository whose workflows they are about. Recorded as A410.
//
// # What a workflow pin is for
//
// Not to restate the YAML. A test that asserted "the `cli` job has these steps"
// against a hand-copied list would fail on every rewording and catch nothing.
// These pin the four things an acceptance criterion names — the job set, the
// timeouts, the release trigger, the e2e gate — and, for the gate, *run* it.
//
// Every comparison is set equality in both directions. "Every expected job is
// present" passes for a workflow with an extra job nobody reviewed, and "every
// present job is expected" passes for a workflow with no jobs at all. PLAN §6
// records the first as the most common defect in this build.

const (
	ciWorkflow      = "../.github/workflows/ci.yml"
	releaseWorkflow = "../.github/workflows/cli-release.yml"
)

type workflow struct {
	Name string `yaml:"name"`

	// `on` is quoted because YAML 1.1 reads the bare word as a boolean, and
	// some parsers still do. yaml.v3 does not, but a pin that depended on
	// that would break silently on a parser change — and "the triggers are
	// exactly `cli/v*`" reading as vacuously true is AC65's whole content.
	// `TestTheWorkflowReaderFindsTriggersAndJobs` is the floor under it.
	On struct {
		Push *struct {
			Branches []string `yaml:"branches"`
			Tags     []string `yaml:"tags"`
		} `yaml:"push"`

		PullRequest *struct {
			Branches []string `yaml:"branches"`
		} `yaml:"pull_request"`

		WorkflowDispatch *struct{} `yaml:"workflow_dispatch"`
		Schedule         []any     `yaml:"schedule"`
	} `yaml:"on"`

	Jobs map[string]job `yaml:"jobs"`
}

type job struct {
	If             string `yaml:"if"`
	Needs          any    `yaml:"needs"`
	TimeoutMinutes int    `yaml:"timeout-minutes"`
	Services       map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"services"`
	Env   map[string]string `yaml:"env"`
	Steps []struct {
		Name string `yaml:"name"`
		Uses string `yaml:"uses"`
		Run  string `yaml:"run"`
	} `yaml:"steps"`
}

func loadWorkflow(t *testing.T, path string) workflow {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var wf workflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	return wf
}

// The floor under every comparison below, as its own example.
//
// `workflow.On` is a struct of pointers, and a parser that stopped populating it
// — the `on:`-as-`true:` problem, a field renamed upstream — would leave every
// member nil. At that point "the release workflow triggers only on `cli/v*`"
// passes by having read nothing, which is exactly AC65 inverted. Separate from
// the comparisons so that "the reader broke" and "the workflow changed" are two
// different failures.
func TestTheWorkflowReaderFindsTriggersAndJobs(t *testing.T) {
	ci := loadWorkflow(t, ciWorkflow)

	if len(ci.Jobs) == 0 {
		t.Errorf("%s parsed to zero jobs", ciWorkflow)
	}

	if ci.On.PullRequest == nil && ci.On.Push == nil {
		t.Errorf("%s parsed to no triggers at all, so the `on:` reader is broken", ciWorkflow)
	}

	release := loadWorkflow(t, releaseWorkflow)

	if release.On.Push == nil {
		t.Fatalf("%s parsed to no push trigger, so every assertion about its tag filter would be "+
			"vacuously true", releaseWorkflow)
	}

	if len(release.On.Push.Tags) == 0 {
		t.Errorf("%s parsed to an empty tag filter; AC65 is entirely about its contents", releaseWorkflow)
	}

	if len(release.Jobs) == 0 {
		t.Errorf("%s parsed to zero jobs", releaseWorkflow)
	}
}

// AC60's job set, as A411 amends it: the `cli` job, and the two the end-to-end
// suite needs now that the app it drives is in another repository.
func TestTheCIWorkflowHasExactlyTheExpectedJobs(t *testing.T) {
	ci := loadWorkflow(t, ciWorkflow)

	assertSetsEqual(t, "jobs in "+ciWorkflow,
		[]string{"cli", "cli-e2e-preflight", "cli-e2e"}, keysOf(ci.Jobs))
}

// PLAN §5.14 and AC60 name both bounds. Mutation M59 removes one.
//
// Every job is checked, not only the two the AC names: an unbounded job holds a
// runner until GitHub's six-hour ceiling, and the pty-driven and money-path
// tests in this build have both hung before.
func TestEveryJobIsTimeBounded(t *testing.T) {
	for _, path := range []string{ciWorkflow, releaseWorkflow} {
		wf := loadWorkflow(t, path)

		if len(wf.Jobs) == 0 {
			t.Fatalf("%s has no jobs, so this check would be vacuous", path)
		}

		for name, j := range wf.Jobs {
			if j.TimeoutMinutes == 0 {
				t.Errorf("%s: job %q declares no timeout-minutes; an unbounded job holds a runner "+
					"for six hours (PLAN §5.14)", path, name)
			}
		}
	}
}

func TestTheDeclaredTimeoutsAreTheOnesThePlanFixes(t *testing.T) {
	ci := loadWorkflow(t, ciWorkflow)
	release := loadWorkflow(t, releaseWorkflow)

	for _, want := range []struct {
		where   string
		job     job
		name    string
		minutes int
	}{
		{ciWorkflow, ci.Jobs["cli"], "cli", 10},
		{ciWorkflow, ci.Jobs["cli-e2e"], "cli-e2e", 20},
		{releaseWorkflow, release.Jobs["release"], "release", 15},
	} {
		if want.job.TimeoutMinutes != want.minutes {
			t.Errorf("%s: job %q has timeout-minutes: %d, want %d (PLAN §5.14, §5.15)",
				want.where, want.name, want.job.TimeoutMinutes, want.minutes)
		}
	}
}

// AC60's `cli` job, by what its steps do.
//
// Step *names* rather than the commands, because the names are the contract with
// a reader of a failed run — and because comparing commands would make this test
// fail on a flag reordering. The commands the acceptance criteria name are
// asserted separately, below, so a step renamed and gutted fails twice.
func TestTheCLIJobRunsExactlyTheExpectedSteps(t *testing.T) {
	ci := loadWorkflow(t, ciWorkflow)

	cli, ok := ci.Jobs["cli"]
	if !ok {
		t.Fatalf("%s has no `cli` job (AC60)", ciWorkflow)
	}

	names := make([]string, 0, len(cli.Steps))
	for _, step := range cli.Steps {
		names = append(names, step.Name)
	}

	assertSetsEqual(t, "steps in the cli job", []string{
		"Checkout",
		"Set up Go",
		"Check formatting",
		"Vet",
		"Test",
		"Test with fault injection",
		"Install goreleaser",
		"Test the release build",
		"Check the module files are unchanged",
	}, names)
}

// The commands PLAN §5.14 and A313 name, as substrings of the job's `run:`
// blocks.
//
// Substring and not equality: `gofmt -l .` is inside a shell conditional and
// `go test` carries flags this test has no business pinning. What it does pin is
// that each command is invoked at all — M59's neighbours are steps deleted
// wholesale, and a deleted step is what this sees.
func TestTheCLIJobInvokesTheCommandsThePlanNames(t *testing.T) {
	ci := loadWorkflow(t, ciWorkflow)

	var script strings.Builder
	for _, step := range ci.Jobs["cli"].Steps {
		script.WriteString(step.Run)
		script.WriteString("\n")
	}

	if script.Len() == 0 {
		t.Fatalf("the cli job has no `run:` steps at all, so every check below would be vacuous")
	}

	for _, command := range []string{
		"gofmt -l .",
		"go vet ./...",
		"go test -count=1 -race ./...",
		"-tags faultinject",
		"-tags goreleaser",
		"git diff --exit-code go.mod go.sum",
	} {
		if !strings.Contains(script.String(), command) {
			t.Errorf("no step in the cli job runs %q (PLAN §5.14)", command)
		}
	}
}

// A411's gate, wired as the workflow says it is.
//
// The three claims: the preflight job runs the script, the e2e job is
// conditional on its output, and the e2e job's condition reads *that* output and
// not something that is always true. The last is the one worth having — `if:
// always()` or a condition on a misspelled output id would make the job skip
// forever, which is the silent-skip failure A411 exists to avoid, arrived at by
// a different route.
func TestTheEndToEndJobIsGatedOnThePreflightScript(t *testing.T) {
	ci := loadWorkflow(t, ciWorkflow)

	preflight, ok := ci.Jobs["cli-e2e-preflight"]
	if !ok {
		t.Fatalf("%s has no `cli-e2e-preflight` job (A411)", ciWorkflow)
	}

	var runs bool
	for _, step := range preflight.Steps {
		if strings.Contains(step.Run, "e2e/ci-preflight.sh") {
			runs = true
		}
	}

	if !runs {
		t.Errorf("the preflight job does not run e2e/ci-preflight.sh. The gate has to be a file, "+
			"because %s cannot be tested and the file is", ciWorkflow)
	}

	e2e, ok := ci.Jobs["cli-e2e"]
	if !ok {
		t.Fatalf("%s has no `cli-e2e` job (AC60)", ciWorkflow)
	}

	if want := "needs.cli-e2e-preflight.outputs.token == 'yes'"; e2e.If != want {
		t.Errorf("the cli-e2e job's condition is %q, want exactly %q. A condition that does not "+
			"read the preflight's output either runs without a token or never runs at all",
			e2e.If, want)
	}

	if !strings.Contains(toString(e2e.Needs), "cli-e2e-preflight") {
		t.Errorf("the cli-e2e job does not `needs: cli-e2e-preflight`, so its `if:` reads an "+
			"output from a job that may not have run: %v", e2e.Needs)
	}
}

// PLAN §5.14's services, and the versions that are not incidental.
func TestTheEndToEndJobHasThePostgresAndRedisThePlanNames(t *testing.T) {
	ci := loadWorkflow(t, ciWorkflow)

	e2e := ci.Jobs["cli-e2e"]

	assertSetsEqual(t, "services in the cli-e2e job",
		[]string{"postgres", "redis"}, keysOf(e2e.Services))

	// 18 for uuidv7(), which every FERRY primary key defaults to; an older
	// Postgres does not boot the schema at all.
	if image := e2e.Services["postgres"].Image; image != "postgres:18" {
		t.Errorf("the cli-e2e job's postgres is %q, want postgres:18 (PLAN §5.14)", image)
	}

	if image := e2e.Services["redis"].Image; !strings.HasPrefix(image, "redis:8") {
		t.Errorf("the cli-e2e job's redis is %q, want redis:8 (PLAN §5.14)", image)
	}
}

// The single reason the suite is a script: the local and CI paths are the same
// path.
func TestTheEndToEndJobRunsTheSameScriptADeveloperRuns(t *testing.T) {
	ci := loadWorkflow(t, ciWorkflow)

	var runs bool
	for _, step := range ci.Jobs["cli-e2e"].Steps {
		if strings.Contains(step.Run, "e2e/run.sh") {
			runs = true
		}
	}

	if !runs {
		t.Errorf("the cli-e2e job does not run e2e/run.sh. PLAN §5.14 described this job as a list " +
			"of steps, which is how the workflow and the README drift; the script is the fix, and " +
			"it only works if the job calls it")
	}

	// The app must be told to use the fixture. A run against a real OMS from
	// CI is the one thing in this workflow that could move money.
	if got := ci.Jobs["cli-e2e"].Env["FERRY_OMS_BACKEND"]; got != "fixture" {
		t.Errorf("the cli-e2e job sets FERRY_OMS_BACKEND=%q, want fixture (PLAN §5.14)", got)
	}
}

// AC65's trigger, as A417 amends it from `cli/v*` to `v*`.
//
// Mutation M71 changes the glob, and the mutation the plan wrote — `cli/v*` to
// `v*` — is now the correct value, so it was run in reverse: `v*` to `cli/v*`.
// Which is the more useful direction anyway, because `cli/v*` is the value
// somebody reading AC65 or PLAN §5.15 would put back, and putting it back
// produces a release whose archives have a `/` in their filenames and a cask
// version of `cli/v0.4.2`.
func TestTheReleaseWorkflowTriggersOnlyOnVersionTags(t *testing.T) {
	release := loadWorkflow(t, releaseWorkflow)

	assertSetsEqual(t, "tag filters in "+releaseWorkflow,
		[]string{"v*"}, release.On.Push.Tags)

	// Named explicitly as well as excluded by the set equality above, because
	// this is the one wrong value with a written justification elsewhere in the
	// repository, and the set-equality failure alone would not say so.
	if toSet(release.On.Push.Tags)["cli/v*"] {
		t.Errorf("%s triggers on `cli/v*`. goreleaser reads the whole tag as the version, so the "+
			"archives become dist/ferry_cli/v… and the Homebrew cask is stamped `version "+
			"\"cli/v0.4.2\"`; there is no OSS way to strip the prefix (A417)", releaseWorkflow)
	}

	// Both directions on the *event* set too. A `workflow_dispatch` would let
	// anybody with write access cut a release from any ref, and a `branches:`
	// filter beside the tags makes every push to that branch a release —
	// which is the same defect as the wrong tag glob, reached by adding
	// rather than by editing.
	if len(release.On.Push.Branches) != 0 {
		t.Errorf("%s also triggers on branches %v; a push to one would cut a release",
			releaseWorkflow, release.On.Push.Branches)
	}

	if release.On.PullRequest != nil {
		t.Errorf("%s triggers on pull_request; a pull request must not cut a release", releaseWorkflow)
	}

	if release.On.WorkflowDispatch != nil {
		t.Errorf("%s triggers on workflow_dispatch; a release must come from a tag, which is the "+
			"only ref goreleaser can derive a version from", releaseWorkflow)
	}

	if len(release.On.Schedule) != 0 {
		t.Errorf("%s triggers on a schedule", releaseWorkflow)
	}
}

// AC65's other half: the release needs the contract digest computed and the tap
// token passed, and both are easy to lose in a rewrite.
func TestTheReleaseJobComputesTheContractDigestAndPassesTheTapToken(t *testing.T) {
	b, err := os.ReadFile(releaseWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflow, err)
	}

	text := string(b)

	for what, needle := range map[string]string{
		"compute the contract digest": "CONTRACT_SHA256=$(sha256sum contract/openapi.yaml",
		"pass the tap token":          "HOMEBREW_TAP_GITHUB_TOKEN",
		"run goreleaser":              "goreleaser/goreleaser-action",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("%s does not %s (looked for %q)", releaseWorkflow, what, needle)
		}
	}
}

// Both scripts must be executable in the tree, or CI fails with a permission
// error and a reader has to work out that `run:` was fine and the mode bit was
// not.
func TestTheShellScriptsAreExecutable(t *testing.T) {
	for _, script := range []string{"run.sh", "ci-preflight.sh"} {
		info, err := os.Stat(script)
		if err != nil {
			t.Fatalf("stat %s: %v", script, err)
		}

		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("e2e/%s is not executable (mode %v); the workflow invokes it directly",
				script, info.Mode().Perm())
		}
	}
}

// The gate, executed. This is the part of A411 that is a measurement rather than
// a claim.
//
// Both branches, because the interesting failures are opposite: a gate that
// always answers `yes` runs the e2e job without a token and it fails on
// checkout; a gate that always answers `no` never runs it again and CI is red
// forever with nobody able to fix it. Each branch reads the answer *and* the
// exit code, since those are two decisions the script makes separately.
func TestThePreflightScriptAnswersBothWays(t *testing.T) {
	for _, tc := range []struct {
		name     string
		token    string
		want     string
		exitCode int
	}{
		{name: "with a token", token: "ghp_notarealtoken", want: "token=yes", exitCode: 0},
		{name: "without one", token: "", want: "token=no", exitCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			outputFile := filepath.Join(dir, "output")
			summaryFile := filepath.Join(dir, "summary")

			script, err := filepath.Abs("ci-preflight.sh")
			if err != nil {
				t.Fatalf("resolve ci-preflight.sh: %v", err)
			}

			cmd := exec.Command(script)
			cmd.Env = append(os.Environ(),
				"FERRY_API_REPO_TOKEN="+tc.token,
				"GITHUB_OUTPUT="+outputFile,
				"GITHUB_STEP_SUMMARY="+summaryFile,
			)

			combined, runErr := cmd.CombinedOutput()

			code := 0
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			} else if runErr != nil {
				t.Fatalf("run ci-preflight.sh: %v", runErr)
			}

			if code != tc.exitCode {
				t.Errorf("ci-preflight.sh exited %d, want %d\n%s", code, tc.exitCode, combined)
			}

			output, err := os.ReadFile(outputFile)
			if err != nil {
				t.Fatalf("the script wrote no $GITHUB_OUTPUT: %v\n%s", err, combined)
			}

			if got := strings.TrimSpace(string(output)); got != tc.want {
				t.Errorf("$GITHUB_OUTPUT is %q, want %q", got, tc.want)
			}
		})
	}
}

// And when it refuses, it says what to do about it.
//
// Separate from the test above because they fail for different reasons and a
// reader of a red CI needs the second one to work: an exit 1 with no
// explanation is indistinguishable from a broken script, and the operator who
// can fix this is not the one who wrote it.
func TestThePreflightRefusalNamesTheSecretAndTheLocalAlternative(t *testing.T) {
	dir := t.TempDir()

	script, err := filepath.Abs("ci-preflight.sh")
	if err != nil {
		t.Fatalf("resolve ci-preflight.sh: %v", err)
	}

	summaryFile := filepath.Join(dir, "summary")

	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(),
		"GITHUB_OUTPUT="+filepath.Join(dir, "output"),
		"GITHUB_STEP_SUMMARY="+summaryFile,
	)
	cmd.Env = append(cmd.Env, "FERRY_API_REPO_TOKEN=")

	combined, _ := cmd.CombinedOutput()

	summary, err := os.ReadFile(summaryFile)
	if err != nil {
		t.Fatalf("the refusal wrote no job summary: %v", err)
	}

	said := string(combined) + string(summary)

	for what, needle := range map[string]string{
		"name the secret an operator must add": "FERRY_API_REPO_TOKEN",
		"name the repository it reads":         "kurenn/ferry",
		"say the suite can be run locally":     "e2e/run.sh",
		"say which criteria are unenforced":    "AC61",
	} {
		if !strings.Contains(said, needle) {
			t.Errorf("the preflight refusal does not %s (looked for %q). A red check whose message "+
				"does not say how to make it green is a check that gets deleted", what, needle)
		}
	}

	// The `::error` annotation is what puts this in the checks UI rather than
	// only in a log nobody opens.
	if !strings.Contains(string(combined), "::error") {
		t.Errorf("the refusal emits no ::error annotation, so it is visible only in the log:\n%s",
			combined)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

// toString renders a `needs:` value, which GitHub allows as either a string or a
// list.
func toString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			parts = append(parts, toString(item))
		}

		return strings.Join(parts, ",")
	default:
		return ""
	}
}
