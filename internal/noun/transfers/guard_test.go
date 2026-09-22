package transfers_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/fixture"
	"github.com/coba-ai/ferry-cli/internal/runs"
	"github.com/coba-ai/ferry-cli/internal/ulid"
)

// C18 and the pre-send refusals. Everything in this file is a case where the
// CLI must send nothing, so every test counts requests at the fixture.

// AC54: `--idempotency-key` reusing a key against a different request is
// refused locally.
//
// FERRY answers that `409 IDEMPOTENCY_KEY_REUSED`, so sending it would be
// safe — and would also be a round trip to learn something already on disk,
// and an attempt whose answer a caller then has to interpret. M57 is "send
// it and let FERRY decide".
func TestAReusedKeyWithADifferentBodyIsRefusedWithoutSending(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	args := append([]string{"transfers", "create", "--idempotency-key", "my-key-1"},
		canonicalSimulateArgs()...)

	stdout, _, exit := run(t, invocation{home: home, args: args})

	if exit != 0 {
		t.Fatalf("the first run exited %d, want 0\n%s", exit, stdout)
	}

	if got := requestsTo(server, simulatePath); got != 1 {
		t.Fatalf("simulate requests = %d, want 1", got)
	}

	// The same key, a different amount.
	changed := make([]string, len(args))
	copy(changed, args)

	for i, a := range changed {
		if a == "100.00" {
			changed[i] = "250.00"
		}
	}

	stdout, stderr, exit := run(t, invocation{home: home, args: changed})

	if exit != 3 {
		t.Fatalf("exit = %d, want 3\n%s\n%s", exit, stdout, stderr)
	}

	// M57.
	if got := requestsTo(server, simulatePath); got != 1 {
		t.Errorf("simulate requests = %d, want 1: the reuse was refused locally", got)
	}

	// The message has to name the run holding the key, or the caller
	// cannot find out what it was used for.
	first := onlyRun(t, home)

	if !strings.Contains(stdout+stderr, first.RunID) {
		t.Errorf("the refusal does not name the run holding the key\n%s\n%s", stdout, stderr)
	}
}

// The control: the *same* key with the *same* request is not refused as a
// conflict, and does not mint a second intent.
//
// A CLI that refused every reuse would be safe and useless — resending the
// identical request under the identical key is exactly what idempotency is
// for, and AC45 depends on it. Without this pair, the check above is
// satisfied by a check on the key alone, which is M56.
//
// What the second invocation does is *route to the run that already holds
// the key* rather than send again. That is the ledger being the authority on
// resend: the answer is already on disk, the simulate settled, and there is
// nothing a second request could add. It is reported as exit 4 because the
// plan token was scrubbed when the step went terminal, so this invocation
// has no plan to show — the caller has to simulate again to get one.
func TestAReusedKeyWithTheIdenticalBodyIsNotRefused(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	args := append([]string{"transfers", "create", "--idempotency-key", "my-key-1"},
		canonicalSimulateArgs()...)

	if _, _, exit := run(t, invocation{home: home, args: args}); exit != 0 {
		t.Fatalf("the first run exited %d, want 0", exit)
	}

	stdout, stderr, exit := run(t, invocation{home: home, args: args})

	// Not 3. Exit 3 is the refusal the *different-body* case gets, and it
	// is the answer this case must not share, because the remedies differ:
	// there a new key is needed, here nothing is wrong at all.
	if exit == 3 {
		t.Fatalf("the identical request under the identical key was refused as a conflict\n%s\n%s",
			stdout, stderr)
	}

	if exit != 4 {
		t.Fatalf("exit = %d, want 4 (the settled run's plan token is scrubbed)\n%s\n%s",
			exit, stdout, stderr)
	}

	// M56 from the other side: no second intent. One run, one key, one
	// request on the wire for the whole pair of invocations.
	all, err := ledgerOf(t, home).List()
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(all) != 1 {
		t.Errorf("%d runs on disk, want 1: a reused key must not mint a second intent", len(all))
	}

	if got := requestsTo(server, simulatePath); got != 1 {
		t.Errorf("simulate requests = %d, want 1: the answer was already on disk", got)
	}

	// And the report points at the run that holds it, so the caller can
	// read what their key already did.
	if !strings.Contains(stdout+stderr, all[0].RunID) {
		t.Errorf("the report does not name the run holding the key\n%s\n%s", stdout, stderr)
	}
}

// AC54's validation: a key that is not printable ASCII is exit 2 and nothing
// is sent.
func TestAnUnusableIdempotencyKeyIsAUsageError(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "a newline", key: "key\n1"},
		{name: "a tab", key: "key\t1"},
		{name: "not ASCII", key: "clé"},
		{name: "too long", key: strings.Repeat("k", 256)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, home := loggedIn(t, "simulate.201")

			stdout, stderr, exit := run(t, invocation{
				home: home,
				args: append([]string{"transfers", "create", "--idempotency-key", tc.key},
					canonicalSimulateArgs()...),
			})

			if exit != 2 {
				t.Errorf("exit = %d, want 2\n%s\n%s", exit, stdout, stderr)
			}

			if got := len(server.Requests()); got != 0 {
				t.Errorf("the fixture saw %d request(s), want 0", got)
			}
		})
	}

	t.Run("an empty value means the flag was not given", func(t *testing.T) {
		// `--idempotency-key ""` is not a key, and refusing it as one
		// would break `--idempotency-key "$KEY"` in a shell script where
		// KEY happens to be unset — which is the ordinary way that
		// expression is written. The run's own derived key is used.
		server, home := loggedIn(t, "simulate.201")

		_, _, exit := run(t, invocation{
			home: home,
			args: append([]string{"transfers", "create", "--idempotency-key", ""},
				canonicalSimulateArgs()...),
		})

		if exit != 0 {
			t.Fatalf("exit = %d, want 0", exit)
		}

		id := onlyRun(t, home).RunID

		if got, want := keysSentTo(server, simulatePath), []string{runs.StepKey(id, runs.StepSimulate)}; got[0] != want[0] {
			t.Errorf("the key sent was %q, want the run's own %q", got[0], want[0])
		}
	})

	t.Run("the boundary is allowed", func(t *testing.T) {
		// 255 bytes, the largest key the check accepts. Without this the
		// table above is satisfied by a check that refuses every key.
		server, home := loggedIn(t, "simulate.201")

		_, _, exit := run(t, invocation{
			home: home,
			args: append([]string{"transfers", "create", "--idempotency-key", strings.Repeat("k", 255)},
				canonicalSimulateArgs()...),
		})

		if exit != 0 {
			t.Errorf("exit = %d, want 0 for a 255-byte key", exit)
		}

		if got := requestsTo(server, simulatePath); got != 1 {
			t.Errorf("simulate requests = %d, want 1", got)
		}
	})
}

// C18: an unresolved run refuses the next money command.
//
// This is the invariant that stops a second transfer being minted for a
// request that may already have been sent. It is asserted across two
// invocations with the fixture as the witness, because the point is that the
// second invocation has no memory of the first beyond the ledger.
func TestAnUnresolvedRunRefusesTheNextMoneyCommand(t *testing.T) {
	server, home := loggedIn(t, "execute.202.then_completed")

	// A 202 followed by a timeout leaves the step `answered`, which is
	// unresolved: FERRY has a command in flight and the outcome is not
	// established.
	_, _, exit := run(t, invocation{
		home:  home,
		clock: frozenClock(),
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes",
			"--timeout", "1s"},
	})

	if exit != 6 {
		t.Fatalf("the arranging run exited %d, want 6", exit)
	}

	orphan := onlyRun(t, home)

	sent := len(server.Requests())

	// M89: ignore the orphan and mint a new key.
	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit != 1 {
		t.Fatalf("exit = %d, want 1: an unresolved run is a local problem and nothing was sent\n%s\n%s",
			exit, stdout, stderr)
	}

	if got := len(server.Requests()); got != sent {
		t.Errorf("the fixture saw %d request(s), want the %d from before: a second key must not "+
			"be minted while the first is unresolved", got, sent)
	}

	// Still one run. A refused command that left a record behind would
	// itself be an orphan, and the next invocation would refuse for a
	// different reason.
	all, err := ledgerOf(t, home).List()
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(all) != 1 {
		t.Errorf("%d runs on disk, want 1", len(all))
	}

	// The refusal has to name the run and the three ways out, or it is a
	// wall.
	report := stdout + stderr

	if !strings.Contains(report, orphan.RunID) {
		t.Errorf("the refusal does not name the unresolved run\n%s", report)
	}

	for _, want := range []string{"runs resume", "runs decline", "--allow-pending"} {
		if !strings.Contains(report, want) {
			t.Errorf("the refusal does not mention %q\n%s", want, report)
		}
	}
}

// AC94, and the reason `route` runs before `orphanCheck` (§5.4 step 4
// before step 5, V3).
//
// A user-supplied key that the ledger already holds against these exact
// bytes **is** a resume of that run. If the orphan check ran first it would
// see the very run the caller is trying to finish and refuse it — the run
// would block itself, and the only way out would be `--allow-pending`,
// which mints a *new* key and sends a second request for a transfer that
// may already be at FERRY. That is the exact accident C18 exists to
// prevent, arrived at by the mechanism meant to prevent it.
//
// M108 is "orphan check before FindByKey", and nothing else in this package
// catches it: the reused-key tests arrange a run that has *settled*, so no
// orphan exists and the order cannot matter. The arrangement here is the
// one where it does.
func TestAKeyThatNamesTheUnresolvedRunRoutesToItRatherThanBeingRefused(t *testing.T) {
	server, home := loggedIn(t, "execute.202.then_completed", "execute.replay.201")

	const key = "u5-ac94-key"

	// A 202 and a timeout leave the step `answered`: unresolved, and an
	// orphan to anything that does not know better.
	_, _, exit := run(t, invocation{
		home:  home,
		clock: frozenClock(),
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes",
			"--idempotency-key", key, "--timeout", "1s"},
	})

	if exit != 6 {
		t.Fatalf("the arranging run exited %d, want 6", exit)
	}

	unresolved := onlyRun(t, home)

	if len(ledgerOrphans(t, home)) != 1 {
		t.Fatalf("the arranged run is not an orphan, so this test cannot distinguish the " +
			"two orders")
	}

	sent := len(server.Requests())

	// The same key, the same bytes.
	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes",
			"--idempotency-key", key},
	})

	report := stdout + stderr

	// Whatever this settles as, it must not be the orphan refusal — and
	// the refusal is recognisable by its guidance, which no other path
	// prints.
	if strings.Contains(report, "--allow-pending") {
		t.Errorf("the run was refused as an orphan by the key that names it. It is now "+
			"unfinishable except by minting a new key, which is a second request for a "+
			"transfer that may already be at FERRY (AC94).\nexit = %d\n%s", exit, report)
	}

	if exit == 1 && strings.Contains(report, "unresolved run") {
		t.Errorf("exit 1 with an unresolved-run message is the orphan refusal\n%s", report)
	}

	// And it routed: the resume resent under the same key, so the fixture
	// saw another request rather than the CLI stopping at a local check.
	if got := len(server.Requests()); got == sent {
		t.Errorf("nothing was sent, so the key did not route to the run it names")
	}

	for _, k := range keysSentTo(server, executePath) {
		if k != key {
			t.Errorf("a request went under the key %q, want %q: a resume sends the "+
				"recorded key and never a new one", k, key)
		}
	}

	// Still one run: routing reuses the record rather than minting.
	all, err := ledgerOf(t, home).List()
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(all) != 1 || all[0].RunID != unresolved.RunID {
		t.Errorf("%d run(s) on disk; the key should have routed to %s rather than minting "+
			"a second", len(all), unresolved.RunID)
	}
}

// ledgerOrphans is the ledger's own view, so a test can say "this
// arrangement really is an orphan" rather than assume it.
func ledgerOrphans(t *testing.T, home string) []runs.Orphan {
	t.Helper()

	found, err := ledgerOf(t, home).Orphans("default", "sandbox")
	if err != nil {
		t.Fatalf("orphans: %v", err)
	}

	return found
}

// The escape hatch works, which is what makes the refusal a safety catch
// rather than a dead end.
//
// The pair also kills the lazy implementation of the test above: a CLI that
// refused every second money command in a profile would pass it.
func TestAllowPendingProceedsPastAnUnresolvedRun(t *testing.T) {
	server, home := loggedIn(t, "execute.202.then_completed", "execute.replay.201")

	_, _, exit := run(t, invocation{
		home:  home,
		clock: frozenClock(),
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes",
			"--timeout", "1s"},
	})

	if exit != 6 {
		t.Fatalf("the arranging run exited %d, want 6", exit)
	}

	before := requestsTo(server, executePath)

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes", "--allow-pending"},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0 with --allow-pending\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != before+1 {
		t.Errorf("execute requests = %d, want %d", got, before+1)
	}

	// Two runs now, each with its own key. That is the point of
	// `--allow-pending`: the caller has said they accept a second intent.
	all, err := ledgerOf(t, home).List()
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(all) != 2 {
		t.Fatalf("%d runs on disk, want 2", len(all))
	}

	if all[0].RunID == all[1].RunID {
		t.Error("the two runs share a run id")
	}
}

// AC58: `--env live` against a sandbox key is refused before anything is
// sent, and the report says which key it found.
func TestTheEnvAssertionRefusesAMismatchedCredential(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create", "--env", "live"}, canonicalSimulateArgs()...),
	})

	if exit != 3 {
		t.Fatalf("exit = %d, want 3\n%s\n%s", exit, stdout, stderr)
	}

	if got := len(server.Requests()); got != 0 {
		t.Errorf("the fixture saw %d request(s), want 0", got)
	}

	report := stdout + stderr

	// Both environments have to appear: what was asked for and what was
	// found. "Environment mismatch" alone leaves the caller guessing which
	// profile they are on.
	for _, want := range []string{"live", "sandbox"} {
		if !strings.Contains(report, want) {
			t.Errorf("the refusal does not mention %q\n%s", want, report)
		}
	}

	// And no secret in it.
	if strings.Contains(report, canaryKey(t)) {
		t.Errorf("the refusal printed the whole token\n%s", report)
	}
}

// The matching assertion passes, and a live key under `--env live` is
// labelled as such.
//
// AC58's second half: a live money command says LIVE where a human will see
// it. The fixture's recordings are sandbox, so this drives the refusal
// against a *live* assertion on a live key — the CLI must get past the
// assertion and then be refused by the fixture's environment match, which is
// proof it did not refuse locally.
func TestALiveKeyPassesTheLiveAssertionAndIsLabelled(t *testing.T) {
	server := fixture.New(t, "simulate.201")
	home := newHome(t)

	writeProfile(t, home, server.URL(), canaryLiveKey(t))

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create", "--env", "live"}, canonicalSimulateArgs()...),
	})

	// The local assertion passed, so a request left. That is the
	// measurement: `--env live` on a live key must not be refused locally.
	//
	// The fixture answers it — A331 makes the *class* the match axis, and
	// a live secret key and a sandbox one are both `api_key` — so the
	// invocation settles normally and the label is what is left to check.
	if got := requestsTo(server, simulatePath); got != 1 {
		t.Fatalf("simulate requests = %d, want 1: --env live on a live key must not be refused "+
			"locally\n%s\n%s", got, stdout, stderr)
	}

	if exit != 0 {
		t.Errorf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	// LIVE, where a human reads it. Not "live" buried in a field name.
	if !strings.Contains(stdout+stderr, "LIVE") {
		t.Errorf("a live money command is not labelled LIVE\n%s\n%s", stdout, stderr)
	}
}

// The other direction for the label: a sandbox command does not say LIVE.
//
// A CLI that printed the banner unconditionally would pass the test above
// and would train every sandbox user to ignore it.
func TestASandboxCommandIsNotLabelledLive(t *testing.T) {
	_, home := loggedIn(t, "simulate.201")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", exit, stdout)
	}

	if strings.Contains(stdout+stderr, "LIVE") {
		t.Errorf("a sandbox command is labelled LIVE\n%s\n%s", stdout, stderr)
	}

	// And the document says which environment it was, so a machine does
	// not have to read the banner.
	stdout, _, _ = run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create", "--output", "json"}, canonicalSimulateArgs()...),
	})

	var doc struct {
		FerryCLI struct {
			Environment *string `json:"environment"`
		} `json:"ferry_cli"`
	}

	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("parse the document: %v\n%s", err, stdout)
	}

	if doc.FerryCLI.Environment == nil || *doc.FerryCLI.Environment != "sandbox" {
		t.Errorf("ferry_cli.environment = %v, want sandbox", doc.FerryCLI.Environment)
	}
}

// A run record is written for the money commands and for nothing else.
//
// A read command that minted a run would put an orphan on disk for every
// `corridors list`, and C18 would then refuse every transfer.
func TestOnlyMoneyCommandsWriteRuns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario string
		args     []string
		wantRuns int
	}{
		{
			name:     "corridors list writes none",
			scenario: "corridors.list.200",
			args:     []string{"corridors", "list"},
			wantRuns: 0,
		},
		{
			name:     "a simulate writes one",
			scenario: "simulate.201",
			args:     append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
			wantRuns: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, home := loggedIn(t, tc.scenario)

			if _, _, exit := run(t, invocation{home: home, args: tc.args}); exit != 0 {
				t.Fatalf("exit = %d, want 0", exit)
			}

			all, err := ledgerOf(t, home).List()
			if err != nil {
				t.Fatalf("list runs: %v", err)
			}

			if len(all) != tc.wantRuns {
				t.Errorf("%d run(s) on disk, want %d", len(all), tc.wantRuns)
			}

			for _, r := range all {
				if !ulid.Valid(r.RunID) {
					t.Errorf("run id %q is not a ULID", r.RunID)
				}
			}
		})
	}
}
