// Package adversarial_test is AC66: every row of PLAN §7.1 is a named test,
// and the set of test names equals the row set — in both directions.
//
// == Why both directions
//
// The bidirectionality is the whole criterion. A census that only asks "does
// every row have a test?" stays green when a row is deleted from §7.1 and its
// test left behind, which is how a table shrinks without anyone noticing. A
// census that only asks "does every test name a row?" stays green when a row
// is added and no test is written, which is how an attack is analysed and
// never defended. Neither direction is worth much alone; this file asserts
// set equality and names both differences in the failure.
//
// That is the defect class `docs/dev-loop-learnings.md` calls a one-directional
// subset check, and A312 is the last time it was found on a control protecting
// consent. This file exists partly because that keeps happening.
//
// == Where the two sets come from
//
// Neither set is typed out here.
//
//   - The row set is parsed out of `docs/PLAN.md` §7.1. A hand-typed list of
//     row ids would be a second authority, and a second authority drifts from
//     the table it claims to mirror — silently, because it is the thing doing
//     the checking.
//   - The test-name set is parsed out of this file's own AST, and then held to
//     what `go test -list` says this package actually registers. The AST alone
//     could be wrong about what compiles; the toolchain alone gives no line
//     numbers. Each is checked against the other, so a broken derivation is a
//     failure rather than a smaller set that passes vacuously.
//
// Both derivations have floors, for the reason `runs.state_census_test.go`
// gives: a parser that stopped recognising anything produces an empty set, and
// an empty set satisfies "every row has a test" perfectly.
//
// == What a row's test actually does
//
// U1 through U6 already wrote tests for the properties these attacks probe.
// AC66 does not ask for them again; it asks that every row be *bound* to the
// test that answers it, in a way that fails when that test is deleted or
// renamed.
//
// The binding is `matrix` below: row id to a list of (package, test name)
// citations. `bind` resolves every citation against the set of tests the Go
// toolchain reports for this module — across the three build-tag
// configurations the suite has, because the money-path crash tests are behind
// `faultinject` and the end-to-end suite behind `e2e`, and a citation that
// silently resolved to nothing under the default tags would be the vacuous
// kind of coverage this unit is auditing.
//
// A citation is therefore checked by the compiler and the test runner, not by
// string equality against a literal in this file. Renaming a cited test breaks
// this file; deleting it breaks this file. That is weaker than re-running the
// cited assertion and stronger than a comment, and the distinction is stated
// rather than glossed: **this file does not verify that a cited test asserts
// anything.** §6.2's mutation table is the control for that, and §11.2 records
// what it measured.
package adversarial_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// The attack table, as PLAN §7.1 writes it
// ---------------------------------------------------------------------------

// minRows is the non-emptiness floor on the parse. §7.1 draws 23 rows; fewer
// than that means the derivation is broken rather than that the analysis
// shrank, and every check below would pass over whatever survived.
const minRows = 23

// planFile is the plan, relative to the module root.
const planFile = "docs/PLAN.md"

// row is one line of §7.1.
type row struct {
	id     string // A1 … A23
	n      int    // the number, for contiguity
	attack string // column 2
	answer string // column 3
	cost   string // column 4
	line   int    // in docs/PLAN.md, so a failure names the line to open
}

// sectionHeading matches the `### 7.1 …` that opens the table and any other
// `##`-or-deeper heading, which closes it.
var (
	headingPattern   = regexp.MustCompile(`^#{2,}\s`)
	attackRowPattern = regexp.MustCompile(`^\|\s*(A(\d+))\s*\|`)
)

// attackTable parses §7.1 out of the plan.
//
// It is deliberately intolerant. A row it cannot split into four columns is a
// failure rather than a skipped line, because a skipped line is a row with no
// test that nothing reports.
func attackTable(t *testing.T) map[string]row {
	t.Helper()

	path := filepath.Join(moduleRoot(t), planFile)

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the plan: %v", err)
	}

	rows, err := parseAttackTable(string(source))
	if err != nil {
		t.Fatalf("parse %s §7.1: %v", planFile, err)
	}

	if len(rows) < minRows {
		t.Fatalf("the parse of %s §7.1 found %d rows; the section draws at least %d.\n\n"+
			"This is the derivation being broken, not the table being short. Every "+
			"assertion in this file is over the set this parse produced, so a smaller "+
			"set does not make them fail — it makes them pass over less.",
			planFile, len(rows), minRows)
	}

	// Contiguity, and no duplicates. §7.1 numbers its rows A1..An with no
	// gaps; a gap means a row was deleted and the ones after it were not
	// renumbered, which would leave this file binding an id that no longer
	// means what its test asserts.
	seen := map[int]row{}
	for _, r := range rows {
		if prev, dup := seen[r.n]; dup {
			t.Errorf("§7.1 has two rows numbered %s, at lines %d and %d", r.id, prev.line, r.line)
		}

		seen[r.n] = r
	}

	for i := 1; i <= len(rows); i++ {
		if _, ok := seen[i]; !ok {
			t.Errorf("§7.1 has %d rows but none numbered A%d; the numbering has a gap, so "+
				"an id in this file may no longer name the row it was written for", len(rows), i)
		}
	}

	byID := map[string]row{}

	for _, r := range rows {
		// A row with no answer is an attack that was written down and not
		// analysed. It cannot be bound to a test, because there is nothing
		// for a test to assert.
		if strings.TrimSpace(r.answer) == "" {
			t.Errorf("§7.1 row %s (%s) at line %d has an empty Answer column", r.id, r.attack, r.line)
		}

		if strings.TrimSpace(r.cost) == "" {
			t.Errorf("§7.1 row %s (%s) at line %d has an empty Cost column", r.id, r.attack, r.line)
		}

		byID[r.id] = r
	}

	return byID
}

// parseAttackTable is the parse, separated from the file read so the control
// below can drive it over fixtures.
func parseAttackTable(source string) ([]row, error) {
	var (
		rows    []row
		inside  bool
		started bool
	)

	for i, line := range strings.Split(source, "\n") {
		switch {
		case strings.HasPrefix(line, "### 7.1"):
			if started {
				return nil, fmt.Errorf("two sections open §7.1; line %d is the second", i+1)
			}

			inside, started = true, true

			continue

		case inside && headingPattern.MatchString(line):
			inside = false

			continue

		case !inside:
			continue
		}

		match := attackRowPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		cells := splitTableRow(line)
		if len(cells) != 4 {
			return nil, fmt.Errorf("line %d (%s) has %d columns, want 4: %q", i+1, match[1], len(cells), line)
		}

		n, err := strconv.Atoi(match[2])
		if err != nil || n < 1 {
			return nil, fmt.Errorf("line %d: %q is not a row number", i+1, match[1])
		}

		rows = append(rows, row{
			id:     match[1],
			n:      n,
			attack: cells[1],
			answer: cells[2],
			cost:   cells[3],
			line:   i + 1,
		})
	}

	if !started {
		return nil, fmt.Errorf("no `### 7.1` heading in the document")
	}

	return rows, nil
}

// splitTableRow splits a GitHub-flavoured table row into its cells.
//
// Cell text contains `|` nowhere in §7.1 and the parse would be wrong if it
// did; that is checked by the column-count assertion above rather than
// assumed, so a row that grows a pipe fails loudly instead of losing a column.
func splitTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")

	cells := strings.Split(trimmed, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}

	return cells
}

// ---------------------------------------------------------------------------
// The bindings
// ---------------------------------------------------------------------------

// cite names one existing test by the package that holds it and its function
// name. Both halves are resolved against the toolchain, so a package that is
// deleted and a test that is renamed each fail here.
type cite struct {
	// pkg is the import path below the module root, e.g. "internal/runs".
	// The module root itself is ".".
	pkg string
	// test is the Go function name, without its `Test` stripped.
	test string
	// why says what this citation contributes to the row's answer. It is
	// prose and nothing checks it; it is here because a citation list with
	// no reasons is a list nobody can audit for the *wrong* test being
	// cited, which is the failure this file cannot detect by itself.
	why string
}

// matrix binds every §7.1 row to the tests that answer it.
//
// The rule for what may appear here: a citation must assert the row's answer,
// not merely touch the same code. Where a row's answer has two halves — a
// local refusal and a server refusal, a state written and a state read — both
// are cited, because a row bound to half of its own answer is the shape A403
// records one level out.
var matrix = map[string][]cite{
	"A1": {
		{"internal/noun/transfers", "TestTheRecordIsOnDiskBeforeTheRequestIsSent",
			"the key and the exact bytes are durable before Do is called, so a kill here leaves them"},
		{"internal/noun/transfers", "TestResumeSendsTheIdenticalRequestUnderTheIdenticalKey",
			"and the resume sends that key rather than minting a fresh one"},
		{"internal/runs", "TestNoFaultInTheWritePathLosesTheKeyWhileTheRequestCouldBeSent",
			"at every fault in the write path, Begin returning nil implies the key is readable"},
	},
	"A2": {
		{"internal/noun/transfers", "TestASendThatLeftAndThenFailedIsPending",
			"a request that left and whose answer never arrived is pending, not 'nothing sent'"},
		{"internal/noun/transfers", "TestResumeSendsTheIdenticalRequestUnderTheIdenticalKey",
			"the resend is the same key and the same bytes, so FERRY replays rather than re-executing"},
		{"internal/noun/transfers", "TestResumingAPendingStepWithNoTerminalIsSix",
			"and a pending step stays pending until the replay answers"},
	},
	"A3": {
		{"internal/noun/transfers", "TestASignalWhilePollingIsPending",
			"an interrupt inside poll.Watch exits 6 and names the command id on stderr"},
		{"internal/noun/transfers", "TestTheCommandIDIsReportedBeforeTheWait",
			"the id is printed before the first wait, so it survives a machine that sleeps mid-poll"},
	},
	"A4": {
		{"internal/noun/transfers", "TestAReusedKeyWithADifferentBodyIsRefusedWithoutSending",
			"AC54: a caller-supplied key the ledger holds against other bytes is refused locally"},
		{"internal/runs", "TestFindByKeyIsAnExactMatch",
			"M8: the lookup is exact, so a near-miss key is not quietly treated as the same transfer"},
	},
	"A5": {
		{"internal/noun/transfers", "TestAnUnresolvedRunRefusesTheNextMoneyCommand",
			"C18/AC81: iteration 2 is refused with zero further requests, and the refusal names the ways out"},
		{"internal/noun/transfers", "TestAllowPendingProceedsPastAnUnresolvedRun",
			"the control: --allow-pending is the written opt-out, so the refusal is not unconditional"},
		{"internal/runs", "TestOrphansReportsExactlyTheUnresolvedStates",
			"and the orphan set is the unresolved states exactly, in both directions"},
	},
	"A6": {
		{"internal/noun/transfers", "TestBroadcastDoesNotResimulateOnARefusal",
			"M46: a 410 PLAN_EXPIRED on execute exits 4 having sent exactly one simulate"},
		{"internal/outcome", "TestRecordedPlanRefusalsAreAllResimulate",
			"every recorded plan refusal classifies refused_resimulate, so none of them exits 0"},
	},
	"A7": {
		{"internal/runs", "TestSecondOpenIsRefusedAfterTheHolderHasRewrittenTheRecord",
			"AC12: the sidecar lock refuses the second holder even across a rewrite of the record"},
		{"internal/runs", "TestSidecarIsNotRenamedByARewrite",
			"M5's other half: the lock is on the sidecar, so a rewrite cannot move it out from under a holder"},
		{"internal/runs", "TestTwoHandlesInOneProcessContend",
			"and the refusal is not an artefact of two processes; one process contends with itself"},
	},
	"A8": {
		{"internal/creds", "TestLoadRefusesAFileWiderThan0600",
			"AC7: a 0644 credentials file is refused"},
		{"internal/creds", "TestLoadDoesNotReadAWideFile",
			"and refused before the bytes are read, so a wide file does not leak through an error path"},
	},
	"A9": {
		{"internal/creds", "TestForRefusesADifferentAPIURLAndWithholdsTheToken",
			"AC9: the stored token is bound to the api_url it was minted against"},
		{"internal/noun", "TestACredentialIsNotHandedToADifferentEndpoint",
			"and the pre-check reaches that refusal, rather than the binding existing only in the store"},
	},
	"A10": {
		{"internal/api", "TestDebugTraceRedactsEverything",
			"AC28: the trace a bug report would carry has no credential in it"},
		{"internal/render", "TestDebugTracesCarryNoSecret",
			"measured over every command that binds the flag, not over one hand-picked invocation"},
	},
	"A11": {
		{"internal/noun/transfers", "TestThePlanTokenCanComeFromStdinAndIsStillTheWholeBody",
			"`--plan -` is the route that keeps the token out of argv, and it produces the same body"},
		{"internal/runs", "TestRecordStripsPlanTokenFromASimulateResponse",
			"AC82: and the token does not come to rest in the ledger once it is off the command line"},
	},
	"A12": {
		{"internal/fixture", "TestLoadAcceptsTheCommittedRecordings",
			"AC77: the loader enforces each scenario's declared `expect`, so a drifted recording fails to load"},
		{"internal/fixture", "TestEveryRecordedInteractionIsServedAsRecorded",
			"and every interaction is replayed as recorded rather than as the CLI would like it"},
		{"internal/api", "TestResponseStructsEqualTheContractSchemas",
			"AC86: the Go structs are pinned to the contract in both directions"},
		{".", "TestTheVendoredRecordingsMatchTheirRecordedDigest",
			"A409: and the vendored bytes are held to a digest, so a recording cannot be edited to agree with the CLI"},
	},
	"A13": {
		{"internal/api", "TestRetryResendsTheSameBytesUnderTheSameKey",
			"AC27: a retried money request is the same key and the same bytes"},
		{"internal/api", "TestRetryStopsAtTheBudget",
			"the budget bounds the storm, and the answer after it still classifies transient/5"},
	},
	"A14": {
		{"internal/outcome", "TestANonEnvelopeBodyIsPendingOnMoneyAtEveryRecordedStatus",
			"AC83: a money answer the CLI cannot read is pending/6 at every recorded status, 201 included"},
		{"internal/api", "TestNonEnvelopeBodiesAreRejected",
			"the envelope decoder refuses rather than filling a zero value that would read as success"},
	},
	"A15": {
		{"internal/noun/transfers", "TestTheEnvAssertionRefusesAMismatchedCredential",
			"AC58: `--env sandbox` over a live key refuses before anything is sent"},
		{"internal/noun/transfers", "TestALiveKeyPassesTheLiveAssertionAndIsLabelled",
			"the control, and the LIVE label a human reads when the assertion is the one they meant"},
		{"internal/noun/auth", "TestEnvRefusesALiveKeyAssertedAsSandbox",
			"AC34: and the same refusal at login, so the profile is never stored mislabelled"},
	},
	"A16": {
		{"internal/noun", "TestEveryOperationRefusesTheWrongClassAndAcceptsTheRight",
			"AC43: the local pre-check refuses a PAT on every money operation, and accepts the right class"},
		{"internal/noun", "TestTheRequirementTableMatchesTheControllers",
			"and the local table is held to what the API's controllers require, so the two cannot disagree"},
		{"e2e", "TestAPATOnlyProfileIsRefusedBeforeAnyRequestLeaves",
			"AC63 against the real FERRY: zero requests leave, so the server's WRONG_TOKEN_CLASS is never needed"},
	},
	"A17": {
		{"internal/noun/transfers", "TestSignalWhileTheRequestIsOnTheWireIsPending",
			"C17/AC69(a): exit 6, step pending, run id on stderr, with the request held on the wire"},
		{"internal/cli/flight", "TestTheFlagStartsFalseAndStaysTrue",
			"the in-flight flag is what makes the exit code 6 rather than 1, and it is monotonic"},
	},
	"A18": {
		{"internal/noun/transfers", "TestAwaitingConfirmationIsNotConsent",
			"C16/AC72: the state written at the prompt is not consent, and a resume may not read it as one"},
		{"internal/noun/transfers", "TestResumeWithNoTerminalRequiresYes",
			"AC73: a resume on a non-TTY sends nothing without --yes"},
		{"internal/consent", "TestAskEstablishesConsentPerInvocation",
			"and consent is per invocation, so the earlier prompt cannot stand in for this one"},
	},
	"A19": {
		{"internal/noun/transfers", "TestResumeRefusesAChangedCredential",
			"C19/AC70: exit 7 with zero requests when the profile's principal no longer matches the record"},
		{"internal/noun/transfers", "TestResumeProceedsWithTheOriginalCredential",
			"the control: the same resume under the original credential does send"},
		{"internal/outcome", "TestIdempotencyKeyReusedSplitsOnDetailsReason",
			"AC71: and if the local check is bypassed, REUSED with different_credential is 7 rather than 3"},
	},
	"A20": {
		{"internal/noun/transfers", "TestARecordFailureAfterTheSendIsPending",
			"AC69(b): a Record that fails after a 201 exits 6 and leaves the step pending"},
		{"internal/fsx", "TestWriteFileAtomicFailureLeavesNothingAtTarget",
			"and a failed write leaves no half-record at the target for the next invocation to read"},
	},
	"A21": {
		{"internal/noun/transfers", "TestBroadcastSimulate202NeverExecutes",
			"AC75/M83: the execute step closes `unreachable`, exit 4, zero execute requests, no null token"},
		{"internal/noun/transfers", "TestBroadcastReplayedSimulateNeverExecutes",
			"the sibling case: a replayed 201 carries no token either, and is refused the same way"},
	},
	"A22": {
		{"internal/noun/transfers", "TestASignalWhilePollingIsPending",
			"AC69(d): an interrupt during the poll after a recorded 202"},
		{"internal/noun/transfers", "TestAPanicAfterTheAnswerWasRecordedIsPending",
			"AC69(c): a renderer panic after a recorded 201 — the other arm of the same claim"},
		{"internal/cli/flight", "TestNothingInThisPackageCanClearTheFlag",
			"V1/P31: monotonicity held against the package's own AST, not against a reading of it"},
		{"internal/noun/transfers", "TestTheSameFaultsBeforeAnySendExitOne",
			"the control: the same faults before any send exit 1, so 6 is not simply what this CLI always says"},
	},
	"A23": {
		{"internal/cli", "TestTheSignalSetIsTheThreeTheDesignNames",
			"M102: SIGHUP is in the listened set, in both directions, so a closed terminal reaches the handler"},
		{"internal/consent", "TestASignalWinsOverABufferedYes",
			"and an interrupt at the prompt is a decline even with a `y` already in the buffer"},
		{"internal/noun/runs", "TestDeclineWritesNoOnlyWhileNoIsStillTrue",
			"AC57/AC92: `runs decline` clears the state without a terminal, for the SIGKILL case"},
		{"internal/runs", "TestDeclineClearsAnAwaitingConfirmationOrphan",
			"V4: and the cleared run stops being an orphan, so C18 no longer blocks the next command"},
	},
}

// ---------------------------------------------------------------------------
// The test-name set, derived from this file
// ---------------------------------------------------------------------------

// rowTestPattern matches the row tests below. The `[A-Z_]` boundary after the
// digits is what keeps `TestA1…` and `TestA13…` apart; without it the greedy
// digit run is ambiguous and A1 would swallow A13's name.
var rowTestPattern = regexp.MustCompile(`^TestA(\d+)([A-Z_].*)$`)

// rowTests derives the row-test names from this file's AST.
//
// It parses the file rather than a list, for the reason A312 gives: a list is
// a second authority, and the second authority is the one that goes stale
// because nothing checks it.
func rowTests(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "adversarial_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parse this file: %v", err)
	}

	found := map[string]string{}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}

		match := rowTestPattern.FindStringSubmatch(fn.Name.Name)
		if match == nil {
			continue
		}

		id := "A" + match[1]

		if prev, dup := found[id]; dup {
			t.Errorf("two functions claim row %s: %s and %s", id, prev, fn.Name.Name)
		}

		found[id] = fn.Name.Name
	}

	if len(found) < minRows {
		t.Fatalf("the AST parse of this file found %d row tests; §7.1 has at least %d rows.\n\n"+
			"A parse that recognises fewer functions than exist would make the set "+
			"equality below hold over a subset, which is the vacuous pass this file "+
			"is about.", len(found), minRows)
	}

	return found
}

// ---------------------------------------------------------------------------
// The census the citations resolve against
// ---------------------------------------------------------------------------

// tagSets is every build configuration the suite has. A citation resolves if
// any of them registers it.
//
// The money-path crash, post-send and resume tests are behind `faultinject`
// and the end-to-end suite behind `e2e`; a census that ran only the default
// configuration would report the tests answering A1, A2, A17, A19, A20 and
// A22 as missing, and a census that *tolerated* a missing citation to avoid
// that would resolve nothing at all.
var tagSets = []string{"", "faultinject", "e2e goreleaser"}

type census struct {
	// byPackage is package path (relative to the module root) to the test
	// names the toolchain registers for it.
	byPackage map[string]map[string]bool
	// packages is every package the toolchain compiled, including those with
	// no tests, so a citation to a real package with a wrong test name is
	// reported differently from one to a package that does not exist.
	packages map[string]bool
}

var (
	censusOnce   sync.Once
	censusResult *census
	censusErr    error
)

// toolchainCensus asks `go test -list` what tests exist.
//
// `-list` compiles every test binary and prints the tests it registers without
// running them. That makes it a check the toolchain performs: a citation to a
// test that no longer compiles, or that was renamed, or whose package was
// deleted, fails here. Parsing the `_test.go` files for `func TestX` would be
// cheaper and would not notice a file excluded by a build tag, a function
// moved behind one, or a package that stopped building.
func toolchainCensus(t *testing.T) *census {
	t.Helper()

	censusOnce.Do(func() {
		censusResult, censusErr = runCensus(moduleRoot(t))
	})

	if censusErr != nil {
		t.Fatalf("census the module's tests: %v", censusErr)
	}

	return censusResult
}

func runCensus(root string) (*census, error) {
	out := &census{
		byPackage: map[string]map[string]bool{},
		packages:  map[string]bool{},
	}

	for _, tags := range tagSets {
		args := []string{"test", "-count=1", "-list", ".*"}
		if tags != "" {
			args = append(args, "-tags", tags)
		}

		args = append(args, "./...")

		cmd := exec.Command("go", args...)
		cmd.Dir = root

		combined, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, combined)
		}

		if err := absorb(out, string(combined)); err != nil {
			return nil, fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
		}
	}

	return out, nil
}

// modulePath is this module, as go.mod declares it. Citations are written
// relative to the module root, so the census strips it.
const modulePath = "github.com/kurenn/ferry-cli"

// absorb reads `go test -list` output.
//
// The format is a run of bare test names followed by a `ok\t<pkg>\t<time>` or
// `?\t<pkg>\t[no test files]` line naming the package they belonged to. Names
// that never reach a package line are a parse failure rather than something to
// drop: dropping them is how a citation resolves against the wrong package.
func absorb(out *census, output string) error {
	var pending []string

	for _, line := range strings.Split(output, "\n") {
		switch {
		case line == "":
			continue

		case strings.HasPrefix(line, "ok  \t"), strings.HasPrefix(line, "?   \t"):
			fields := strings.Split(line, "\t")
			if len(fields) < 2 {
				return fmt.Errorf("cannot read the package out of %q", line)
			}

			pkg := relativePackage(fields[1])
			out.packages[pkg] = true

			if out.byPackage[pkg] == nil {
				out.byPackage[pkg] = map[string]bool{}
			}

			for _, name := range pending {
				out.byPackage[pkg][name] = true
			}

			pending = nil

		case strings.HasPrefix(line, "Test"), strings.HasPrefix(line, "Benchmark"),
			strings.HasPrefix(line, "Example"), strings.HasPrefix(line, "Fuzz"):
			pending = append(pending, line)

		default:
			// `go test -list` prints nothing else on a clean run. A
			// FAIL line, a build error or a vet complaint lands here,
			// and each means the census is incomplete.
			return fmt.Errorf("unexpected line %q; the census would be incomplete", line)
		}
	}

	if len(pending) > 0 {
		return fmt.Errorf("%d test names (%v …) were printed with no package line after them",
			len(pending), pending[0])
	}

	return nil
}

func relativePackage(importPath string) string {
	if importPath == modulePath {
		return "."
	}

	return strings.TrimPrefix(importPath, modulePath+"/")
}

// ---------------------------------------------------------------------------
// bind — what each row test does
// ---------------------------------------------------------------------------

// bind is the per-row assertion.
//
// It ties three things together: the row must exist in §7.1, the row must have
// citations in `matrix`, and every citation must name a test the toolchain can
// see. A row test that called nothing would satisfy the set-equality check
// above while asserting nothing, so every row test calls this and the set
// equality alone is never the whole claim.
func bind(t *testing.T, id string) {
	t.Helper()

	table := attackTable(t)

	if _, ok := table[id]; !ok {
		t.Fatalf("this test claims §7.1 row %s, which the table does not have.\n\n"+
			"Either the row was deleted and this test should go with it, or it was "+
			"renumbered — in which case this test now asserts the citations of one "+
			"row against the answer of another.", id)
	}

	cites := matrix[id]
	if len(cites) == 0 {
		t.Fatalf("§7.1 row %s has no citation in `matrix`, so nothing binds it to a test", id)
	}

	known := toolchainCensus(t)

	for _, c := range cites {
		if !known.packages[c.pkg] {
			t.Errorf("row %s cites %s in package %q, which this module does not build.\n\n"+
				"Packages the toolchain reported: %s",
				id, c.test, c.pkg, strings.Join(sortedKeys(known.packages), ", "))

			continue
		}

		if !known.byPackage[c.pkg][c.test] {
			t.Errorf("row %s cites %s in %s, which is not a test that package registers "+
				"under any of the build tags %q.\n\n"+
				"The citation says it covers: %s\n\n"+
				"A renamed test is the common cause. Re-point the citation at whatever "+
				"asserts the row's answer now, or — if nothing does — write it, because "+
				"the row is then an attack this CLI has no answer for.",
				id, c.test, c.pkg, tagSets, c.why)
		}
	}
}

// ---------------------------------------------------------------------------
// AC66 itself
// ---------------------------------------------------------------------------

// TestTheAttackTableAndTheseTestsAreTheSameSet is AC66.
//
// Both differences are named. The failure message for each direction says what
// the direction means, because the two are fixed differently: a row with no
// test needs a test, and a test with no row needs either the row restored or
// the test deleted, and getting that backwards is how a deleted row's test
// survives as a test of nothing.
func TestTheAttackTableAndTheseTestsAreTheSameSet(t *testing.T) {
	table := attackTable(t)
	tests := rowTests(t)

	for id, r := range table {
		if _, ok := tests[id]; !ok {
			t.Errorf("§7.1 row %s is not a named test in this file.\n\n"+
				"  attack: %s\n  answer: %s\n  %s:%d\n\n"+
				"Add `func Test%s…`. If the row's answer is not asserted anywhere in "+
				"this repository, that is the finding — record it rather than binding "+
				"the row to a test that passes for another reason.",
				id, r.attack, r.answer, planFile, r.line, id)
		}
	}

	for id, name := range tests {
		if _, ok := table[id]; !ok {
			t.Errorf("%s names §7.1 row %s, which the table does not have.\n\n"+
				"A test for a row that was deleted is a test nobody can evaluate: its "+
				"name asserts the analysis still contains a claim it no longer contains. "+
				"Restore the row or delete the test.", name, id)
		}
	}

	if len(table) != len(tests) {
		t.Errorf("§7.1 has %d rows and this file has %d row tests", len(table), len(tests))
	}
}

// TestTheMatrixCoversExactlyTheAttackTable holds the citation table to §7.1 in
// both directions too.
//
// Without it, `matrix` could accumulate an entry for a deleted row — harmless
// in itself, but it means the citation list and the table have diverged, and
// the citation list is what a reader consults to find out whether a row is
// covered.
func TestTheMatrixCoversExactlyTheAttackTable(t *testing.T) {
	table := attackTable(t)

	for id := range table {
		if len(matrix[id]) == 0 {
			t.Errorf("§7.1 row %s has no entry in `matrix`", id)
		}
	}

	for id := range matrix {
		if _, ok := table[id]; !ok {
			t.Errorf("`matrix` binds row %s, which §7.1 does not have", id)
		}
	}
}

// TestNoCitationIsRepeatedWithinARow catches the copy-paste that makes a row
// look better covered than it is: three citations, two of them the same test.
func TestNoCitationIsRepeatedWithinARow(t *testing.T) {
	for id, cites := range matrix {
		seen := map[cite]bool{}

		for _, c := range cites {
			key := cite{pkg: c.pkg, test: c.test}
			if seen[key] {
				t.Errorf("row %s cites %s/%s twice", id, c.pkg, c.test)
			}

			seen[key] = true

			if strings.TrimSpace(c.why) == "" {
				t.Errorf("row %s cites %s/%s with no reason; a citation nobody can audit "+
					"is a citation that can be wrong without being noticed", id, c.pkg, c.test)
			}
		}
	}
}

// TestTheTestNameDerivationAgreesWithTheToolchain holds the AST derivation to
// what actually gets registered.
//
// The AST is what the set equality above is computed over. If it disagreed
// with the toolchain — a function the parse missed, or one it invented from a
// name in a comment — the equality would be over the wrong set and would still
// pass. Two independent derivations agreeing is what makes either usable, the
// same argument A409 makes for checking the Go digest against the shell
// pipeline.
func TestTheTestNameDerivationAgreesWithTheToolchain(t *testing.T) {
	fromAST := rowTests(t)
	registered := toolchainCensus(t).byPackage["internal"]

	if len(registered) == 0 {
		t.Fatalf("the toolchain reports no tests for package `internal`; the comparison "+
			"below would be vacuous. Packages seen: %s",
			strings.Join(sortedKeys(toolchainCensus(t).packages), ", "))
	}

	for id, name := range fromAST {
		if !registered[name] {
			t.Errorf("the AST derivation found %s for row %s, which `go test -list` does "+
				"not register. The parse and the compiler disagree about this file.", name, id)
		}
	}

	for name := range registered {
		match := rowTestPattern.FindStringSubmatch(name)
		if match == nil {
			continue // A census test, not a row test.
		}

		if _, ok := fromAST["A"+match[1]]; !ok {
			t.Errorf("`go test -list` registers %s, which the AST derivation did not find. "+
				"The derivation is missing functions, so the set equality is over a subset.", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Controls on the machinery above
// ---------------------------------------------------------------------------

// The citation resolver must actually refuse a name that is not there.
//
// Every binding in this file rests on it. If `bind` resolved everything — a
// census that silently came back empty, a package key that never matched —
// then all 23 rows would pass while citing nothing, which is precisely the
// vacuous control this unit is auditing for elsewhere.
func TestTheCensusRefusesATestThatIsNotThere(t *testing.T) {
	known := toolchainCensus(t)

	// A real package, so this isolates the test-name half.
	const pkg = "internal/runs"

	if !known.packages[pkg] {
		t.Fatalf("%s is not in the census, so this control cannot distinguish the two halves", pkg)
	}

	if known.byPackage[pkg]["TestThisNameIsNotADeclaredTestAnywhereInThisModule"] {
		t.Error("the census resolved a fabricated test name")
	}

	if known.packages["internal/nosuchpackage"] {
		t.Error("the census resolved a package that does not exist")
	}

	// And the positive half, so a census that refused *everything* is also
	// caught. A resolver that always answers false passes the two checks
	// above and fails every real citation for the wrong reason.
	if !known.byPackage[pkg]["TestFindByKeyIsAnExactMatch"] {
		t.Error("the census did not resolve a test that is certainly there; it is refusing " +
			"everything, which makes the two checks above meaningless")
	}
}

// The §7.1 parser must refuse tables it cannot read, rather than returning the
// rows it managed.
//
// A parser that silently dropped a malformed row would shrink the row set, and
// a smaller row set is not a failure of the set equality — it is a smaller
// claim that passes.
func TestTheAttackTableParserRefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		wantErr string
		wantIDs []string
	}{
		{
			name: "the shape §7.1 actually has",
			source: "### 7.1 The attack table\n" +
				"\n| # | Attack | Answer | Cost |\n| --- | --- | --- | --- |\n" +
				"| A1 | kill -9 | pending | One request. |\n" +
				"| A2 | lid closed | exit 6 | Nothing. |\n",
			wantIDs: []string{"A1", "A2"},
		},
		{
			name:    "a row that lost a column",
			source:  "### 7.1 x\n| A1 | kill -9 | pending |\n",
			wantErr: "has 3 columns",
		},
		{
			name: "rows after the section ends are not rows of it",
			source: "### 7.1 x\n| A1 | a | b | c |\n" +
				"### 7.2 What I could not close\n| A2 | a | b | c |\n",
			wantIDs: []string{"A1"},
		},
		{
			name:    "no section at all",
			source:  "## 7 Adversarial analysis\n| A1 | a | b | c |\n",
			wantErr: "no `### 7.1` heading",
		},
		{
			name: "two sections claiming to be §7.1",
			source: "### 7.1 x\n| A1 | a | b | c |\n" +
				"## 8\n### 7.1 x again\n| A2 | a | b | c |\n",
			wantErr: "two sections open",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := parseAttackTable(tc.source)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parsed %d rows, want the error %q", len(rows), tc.wantErr)
				}

				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q, want it to contain %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			var got []string
			for _, r := range rows {
				got = append(got, r.id)
			}

			if strings.Join(got, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("rows %v, want %v", got, tc.wantIDs)
			}
		})
	}
}

// The row-id pattern must keep A1 and A13 apart.
//
// Without the `[A-Z_]` boundary the digit run is greedy in a way that still
// matches, and `TestA1ThreeThingsHappen` would be read as row A1 — or worse,
// a careless `(\d)` would read `TestA13…` as A1 and silently give row A13's
// binding to row A1. Both sets would still have 23 members and the equality
// would pass.
func TestTheRowIDPatternIsNotAmbiguous(t *testing.T) {
	cases := map[string]string{
		"TestA1AKillBetweenTheKeyAndTheSend": "A1",
		"TestA13ARateLimitStorm":             "A13",
		"TestA2":                             "", // no descriptive half
		"TestAllStatesIsComplete":            "", // not a row test
		"TestA23ATerminalThatGoesAway":       "A23",
	}

	for name, want := range cases {
		match := rowTestPattern.FindStringSubmatch(name)

		got := ""
		if match != nil {
			got = "A" + match[1]
		}

		if got != want {
			t.Errorf("%s read as %q, want %q", name, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The rows
// ---------------------------------------------------------------------------

func TestA1AKillBetweenTheKeyBeingWrittenAndTheRequestLeaving(t *testing.T) { bind(t, "A1") }

func TestA2AKillBetweenTheRequestLeavingAndTheAnswerBeingRecorded(t *testing.T) { bind(t, "A2") }

func TestA3AMachineThatSleepsInTheMiddleOfAPoll(t *testing.T) { bind(t, "A3") }

func TestA4AnIdempotencyKeyBorrowedFromADifferentTransfer(t *testing.T) { bind(t, "A4") }

func TestA5AWrapperThatRerunsTheCommandLineOnExitSixOrSeven(t *testing.T) { bind(t, "A5") }

func TestA6AHumanWhoConfirmsAfterThePlanHasExpired(t *testing.T) { bind(t, "A6") }

func TestA7TwoTerminalsResumingOneRun(t *testing.T) { bind(t, "A7") }

func TestA8ACredentialsFileAnyoneOnTheBoxCanRead(t *testing.T) { bind(t, "A8") }

func TestA9AStoredTokenPointedAtSomebodyElsesEndpoint(t *testing.T) { bind(t, "A9") }

func TestA10ADebugTracePastedIntoABugReport(t *testing.T) { bind(t, "A10") }

func TestA11APlanTokenLeftInTheProcessTable(t *testing.T) { bind(t, "A11") }

func TestA12AFixtureThatHasDriftedFromTheAPI(t *testing.T) { bind(t, "A12") }

func TestA13ARateLimitStormOnAMoneyRequest(t *testing.T) { bind(t, "A13") }

func TestA14ASuccessfulStatusCarryingABodyTheCLICannotRead(t *testing.T) { bind(t, "A14") }

func TestA15ALiveKeyUnderAProfileAScriptThoughtWasSandbox(t *testing.T) { bind(t, "A15") }

func TestA16APersonalAccessTokenOnAMoneyCommand(t *testing.T) { bind(t, "A16") }

func TestA17AnInterruptWhileTheExecuteRequestIsOnTheWire(t *testing.T) { bind(t, "A17") }

func TestA18AnInterruptAtThePromptFollowedByAResume(t *testing.T) { bind(t, "A18") }

func TestA19AResumeOfAnOlderRunAfterTheCredentialChanged(t *testing.T) { bind(t, "A19") }

func TestA20ADiskThatFillsUpBetweenTheAnswerAndTheRecord(t *testing.T) { bind(t, "A20") }

func TestA21ASimulateThatAnswers202UnderBroadcast(t *testing.T) { bind(t, "A21") }

func TestA22AnAbnormalExitAfterTheAnswerWasAlreadyRecorded(t *testing.T) { bind(t, "A22") }

func TestA23ATerminalThatGoesAwayWhileTheQuestionIsOpen(t *testing.T) { bind(t, "A23") }

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// moduleRoot walks up from this package to the directory holding go.mod.
//
// A404 is the reason this is not a relative literal: these tests run from a
// git worktree as often as from a plain clone, and a path assembled from
// `../` happens to work in both only by accident of depth.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above the working directory")
		}

		dir = parent
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
