//go:build e2e

package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC63 — the refusals, against the real app.
//
// Defends C4 and C15. Three of AC63's four clauses are here. The fourth is not
// implementable: see `TestTheHiddenNoPrecheckFlagDoesNotExist` at the bottom,
// which is what stops the gap being silent.

// AC63's first clause. A PAT cannot simulate or execute, and the CLI knows the
// same per-endpoint credential table FERRY does, so it refuses **locally**:
// `http == null`, `outcome.class == "refused_fix"`, and nothing on the wire.
//
// `http == null` is the whole assertion and the reason `document.HTTP` is a
// pointer. A CLI that emitted `http: {}` here would satisfy "there is an http
// member" and tell a reader a request was made (mutation M116).
func TestAPATOnlyProfileIsRefusedBeforeAnyRequestLeaves(t *testing.T) {
	c := newCLI(t)
	c.login()

	// Deliberately no `keys create --login`: this profile holds a PAT and
	// nothing else, which is the state a new operator is in.
	tr := newTransfer(t)

	doc, res := c.json(append([]string{"transfers", "create"}, tr.flags()...)...)

	if res.exitCode != 3 {
		t.Fatalf("a PAT-only profile exited %d, want 3\nstdout:\n%s\nstderr:\n%s",
			res.exitCode, res.stdout, res.stderr)
	}

	if doc.HTTP != nil {
		t.Errorf("http is %+v, want null. The pre-check decides this from the token's own class, "+
			"so a request must never have left", *doc.HTTP)
	}

	if doc.Outcome.Class != "refused_fix" {
		t.Errorf("outcome.class is %q, want refused_fix", doc.Outcome.Class)
	}

	// No run either: a command refused before it read the profile mints no
	// ledger entry, and one left behind would block the next invocation.
	if doc.FerryCLI.RunID != nil {
		t.Errorf("the refusal minted run %s; nothing was sent, so there is nothing to resume",
			*doc.FerryCLI.RunID)
	}

	if runs := c.pendingRuns(); len(runs) != 0 {
		t.Errorf("the refusal left %d unresolved run(s): %v", len(runs), runs)
	}
}

// AC63's third clause. A live key carrying `money:execute` needs step-up, which
// FERRY does not implement, so the answer is 403 STEP_UP_REQUIRED and the exit
// code is 3 — "refused, fix something and try again" — not 1 and not 5.
func TestALiveKeyCarryingMoneyExecuteIsRefusedWithStepUpRequired(t *testing.T) {
	c := newCLI(t)
	c.login()

	doc, res := c.json("keys", "create", "--env", "live", "--name", "cli-e2e-live", "--scopes", "money:execute")

	if res.exitCode != 3 {
		t.Fatalf("keys create --env live --scopes money:execute exited %d, want 3\nstdout:\n%s\nstderr:\n%s",
			res.exitCode, res.stdout, res.stderr)
	}

	assertAPIError(t, doc, 403, "STEP_UP_REQUIRED")
}

// AC63's fourth clause. `--body` is forwarded verbatim (AC56), so a document
// with a key FERRY does not know is refused by FERRY rather than canonicalised
// away by the CLI — which is the point: a caller who has a body FERRY accepts
// must be able to send exactly it, and a caller who has one it rejects must see
// the rejection.
func TestABodyWithAnUnknownKeyIsRefusedByFERRYWithValidationFailed(t *testing.T) {
	c := newCLI(t)
	c.login()
	c.mintSandboxKey()

	tr := newTransfer(t)

	path := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(path, unknownKeyBody(t, tr), 0o600); err != nil {
		t.Fatalf("write the body: %v", err)
	}

	doc, res := c.json("transfers", "create", "--body", path)

	if res.exitCode != 3 {
		t.Fatalf("a body with an unknown key exited %d, want 3\nstdout:\n%s\nstderr:\n%s",
			res.exitCode, res.stdout, res.stderr)
	}

	assertAPIError(t, doc, 400, "VALIDATION_FAILED")
}

// unknownKeyBody is a TransferRequest FERRY would otherwise accept, with one
// member it does not declare.
//
// Written as bytes rather than marshalled from a map because `--body` is sent
// verbatim and a map's key order is not stable: the assertion is about the
// unknown key, and the body should differ from a valid one in exactly that.
func unknownKeyBody(t *testing.T, tr transfer) []byte {
	t.Helper()

	return []byte(`{
  "customer_id": "` + tr.customer + `",
  "source": {"type": "walletCrypto", "id": "` + tr.source + `", "asset": "usdc", "network": "polygon"},
  "destination": {"type": "bankUs", "id": "` + tr.destination + `", "asset": "usd", "network": "ach", "account_holder": "customer"},
  "amount": {"side": "source", "value": "` + tr.amount + `"},
  "not_a_declared_member": "x"
}`)
}

// AC63's second clause cannot be driven, and this is the record of it.
//
// AC63 asks for a hidden `--no-precheck` that sends the request the pre-check
// would have refused, so that FERRY's own `403 WRONG_TOKEN_CLASS` is exercised
// end to end and the CLI's local table is shown to agree with the server's.
// No unit implemented the flag: it is in neither `bindMoneyFlags`
// (`internal/noun/transfers/execute.go`) nor `BindGlobals`
// (`internal/noun/precheck.go`), and neither `ferry --help` nor `ferry transfers
// create --help` mentions it.
//
// This test asserts the absence rather than skipping the clause, for two
// reasons. A skip would leave AC63 reading as met. And when somebody does add
// the flag, this test fails and points at the branch that is now writable —
// which is the only form of to-do that cannot be forgotten.
//
// Recorded as A413, Open.
func TestTheHiddenNoPrecheckFlagDoesNotExist(t *testing.T) {
	c := newCLI(t)

	for _, help := range [][]string{
		{"--help"},
		{"transfers", "create", "--help"},
		{"transfers", "execute", "--help"},
	} {
		res := c.run(help...)

		// Cobra writes a group's help to stderr and a leaf's to stdout, so
		// both are searched.
		if strings.Contains(res.stdout+res.stderr, "no-precheck") {
			t.Errorf("`ferry %v` now documents --no-precheck. AC63's second clause is implementable: "+
				"drive it with a PAT-only profile and assert 403 WRONG_TOKEN_CLASS and exit 3, then "+
				"close A413 and delete this test", help)
		}
	}
}

// assertAPIError holds a refusal to the status and code FERRY answered with.
//
// Both, always. The status alone would pass for a 403 with any code — and
// "refused for some reason" is not what AC63 claims; it names the code because
// the code is what tells an operator whether to change the credential or the
// request.
func assertAPIError(t *testing.T, doc document, status int, code string) {
	t.Helper()

	if doc.HTTP == nil {
		t.Fatalf("http is null, so FERRY was never asked; want a %d %s", status, code)
	}

	if doc.HTTP.Status != status {
		t.Errorf("FERRY answered %d, want %d", doc.HTTP.Status, status)
	}

	if doc.Error == nil {
		t.Fatalf("the document carries no error envelope; want code %q", code)
	}

	if doc.Error.Code != code {
		t.Errorf("error.code is %q, want %q", doc.Error.Code, code)
	}

	if doc.Outcome.ExitCode != 3 {
		t.Errorf("outcome.exit_code is %d, want 3", doc.Outcome.ExitCode)
	}
}
