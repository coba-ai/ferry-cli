//go:build e2e

package e2e_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// AC61 — the happy path, against a real FERRY with FERRY_OMS_BACKEND=fixture.
//
// Defends C1 (money moves only where the CLI says it does), C6 and C9.
//
// One test per clause rather than one long script, because the clauses fail for
// different reasons and a single test would report the first and hide the rest.
// The two that need a priced plan share the whole path up to it, which is what
// `priced` is.

// planTokenPattern is the token AC61 requires on stdout, and the canary the
// scrub assertions look for. `ferry_plan_` followed by the base62 body the API
// mints (`ferry_plan_OgyjyhazeRx7h6B7sDevpLL962mCT89R3Gfv20rrFCh` measured).
var planTokenPattern = regexp.MustCompile(`ferry_plan_[0-9A-Za-z]{20,}`)

// slots is what `auth whoami` answers: a census of this profile's two
// credential slots, taken locally.
//
// Worth stating because it caught this file out. `whoami` sends nothing —
// `whoami.go`'s own comment gives the reason, that `GET /v1/me` could only
// speak for whichever slot was presented and so would answer a narrower
// question than the one asked — and the first draft of these tests asserted a
// `kind` and a 200 that no `whoami` has ever produced. The endpoint is called by
// `auth login`, which is where the 200 is asserted below.
type slots struct {
	Profile string `json:"profile"`
	APIURL  string `json:"api_url"`

	APIKey *slot `json:"api_key"`
	PAT    *slot `json:"pat"`

	Stored  []string `json:"stored"`
	Missing []string `json:"missing"`
}

type slot struct {
	TokenPrefix string  `json:"token_prefix"`
	TokenLast4  string  `json:"token_last4"`
	Environment *string `json:"environment"`
	PrincipalID string  `json:"principal_id"`
}

func (c *cli) whoami() (slots, result) {
	c.t.Helper()

	doc, res := c.json("auth", "whoami")
	if res.exitCode != 0 {
		c.t.Fatalf("whoami exited %d\nstdout:\n%s\nstderr:\n%s", res.exitCode, res.stdout, res.stderr)
	}

	var census slots
	unmarshal(c.t, doc.Response, &census)

	// The census names both slots twice over — as a `stored`/`missing` pair
	// and as two nullable members — and a document where those disagree is one
	// a caller cannot script against. Checked on every read rather than once,
	// because it is cheap and it is the invariant the shape exists for.
	if (census.APIKey != nil) != toSet(census.Stored)[credentialAPIKey] {
		c.t.Errorf("whoami's api_key member and its `stored` list disagree: %+v / %v",
			census.APIKey, census.Stored)
	}

	if (census.PAT != nil) != toSet(census.Stored)[credentialPAT] {
		c.t.Errorf("whoami's pat member and its `stored` list disagree: %+v / %v",
			census.PAT, census.Stored)
	}

	return census, res
}

// The two slot names, as `creds.Class` spells them on the wire.
const (
	credentialAPIKey = "api_key"
	credentialPAT    = "pat"
)

// AC61's first clause. `auth login` verifies against `GET /v1/me` and stores the
// PAT only if that answered 200.
//
// The "only" is the half worth measuring, and it needs a second invocation with
// a token FERRY rejects: a test that logged in successfully and found a stored
// credential cannot tell "stored because the call succeeded" from "stored
// regardless". So both, in one test, because the second is meaningless without
// the first having established what success looks like.
func TestLoginStoresAPATOnlyAfterGetMeAnswers200(t *testing.T) {
	c := newCLI(t)

	res := c.exec(c.bin, nil, c.pat+"\n", "auth", "login", "--token-stdin", "--api", c.apiURL, "--output", "json")
	if res.exitCode != 0 {
		t.Fatalf("auth login exited %d\nstdout:\n%s\nstderr:\n%s", res.exitCode, res.stdout, res.stderr)
	}

	login := c.parse(res, []string{"auth", "login"})

	if login.HTTP == nil || login.HTTP.Status != 200 {
		t.Fatalf("auth login did not call GET /v1/me and get a 200: http=%+v", login.HTTP)
	}

	census, whoami := c.whoami()

	assertSetsEqual(t, "credential slots after logging in with a PAT",
		[]string{credentialPAT}, census.Stored)

	if census.PAT == nil {
		t.Fatalf("no PAT is stored after a successful login")
	}

	// AC36/AC42: the prefix and the last four identify the credential and the
	// secret does not appear. Asserted here as well as in U4's unit tests
	// because this is the only place the token is a *real* one, and a
	// redaction that worked on `ferry_pat_xxx` and not on the mint's own
	// output would pass there and fail here.
	if census.PAT.TokenPrefix == "" || census.PAT.TokenLast4 == "" {
		t.Errorf("the stored PAT has prefix %q and last four %q; there is nothing identifying it",
			census.PAT.TokenPrefix, census.PAT.TokenLast4)
	}

	for what, output := range map[string]string{
		"`auth login`":  res.stdout + res.stderr,
		"`auth whoami`": whoami.stdout + whoami.stderr,
	} {
		if strings.Contains(output, c.pat) {
			t.Errorf("%s printed the personal access token itself", what)
		}
	}

	// And the "only": a token FERRY will not accept must leave the profile as
	// it was. Run in a profile of its own so that a login which wrongly stored
	// it could not be masked by the successful one above.
	rejected := newCLI(t)

	bad := rejected.exec(rejected.bin, nil, "ferry_pat_"+strings.Repeat("A", 43)+"\n",
		"auth", "login", "--token-stdin", "--api", rejected.apiURL, "--output", "json")

	if bad.exitCode == 0 {
		t.Fatalf("logging in with a token FERRY does not know exited 0:\n%s", bad.stdout)
	}

	// "The profile holds nothing" is read off `whoami`'s refusal rather than
	// off its census, because an empty profile has no census: `whoami` runs the
	// same `RequireEither` pre-check every other command does and exits 3 with
	// `http == null` (whoami.go's comment says why — a profile holding neither
	// credential has no answer to give and should say so in the vocabulary
	// every other command uses). That refusal *is* the assertion.
	after, res := rejected.json("auth", "whoami")

	if res.exitCode != 3 {
		t.Errorf("after a rejected login, `whoami` exited %d rather than refusing with 3. "+
			"`auth login` verifies before it writes, and a credential stored on a 401 is one "+
			"every later command presents and every later command is refused for\nstdout:\n%s",
			res.exitCode, res.stdout)
	}

	if after.HTTP != nil {
		t.Errorf("`whoami` on an empty profile reports http %+v; it sends nothing", *after.HTTP)
	}
}

func TestKeysCreateWithLoginStoresAMintedSandboxKey(t *testing.T) {
	c := newCLI(t)
	c.login()

	doc := c.mintSandboxKey()

	if doc.HTTP == nil || doc.HTTP.Status != 201 {
		t.Fatalf("keys create did not answer 201: http=%+v", doc.HTTP)
	}

	var key struct {
		Scopes      []string `json:"scopes"`
		Environment struct {
			Kind string `json:"kind"`
		} `json:"environment"`
		Token string `json:"token"`
	}

	unmarshal(t, doc.Response, &key)

	if key.Environment.Kind != "sandbox" {
		t.Errorf("the minted key is bound to %q, not sandbox — `--env sandbox` did not reach the request",
			key.Environment.Kind)
	}

	// Set equality in both directions on the scopes, because the interesting
	// failure is a scope the CLI silently dropped or one the API silently
	// added. A subset check in either direction passes for one of those.
	assertSetsEqual(t, "scopes on the minted key",
		[]string{"read", "money:simulate", "money:execute"}, key.Scopes)

	if key.Token == "" {
		t.Fatalf("the 201 carried no token, so `--login` had nothing to store")
	}

	// `--login` wrote it into the other slot, leaving the PAT alone. Both
	// directions: a `--login` that replaced the PAT would pass a check for
	// "the api_key slot is filled", and the PAT is what mints the next key.
	census, res := c.whoami()

	assertSetsEqual(t, "credential slots after `keys create --login`",
		[]string{credentialAPIKey, credentialPAT}, census.Stored)

	if census.APIKey == nil {
		t.Fatalf("no API key is stored after `keys create --login`")
	}

	if census.APIKey.Environment == nil || *census.APIKey.Environment != "sandbox" {
		t.Errorf("the stored key's environment is %v, want sandbox", census.APIKey.Environment)
	}

	if strings.Contains(res.stdout+res.stderr, key.Token) {
		t.Errorf("`auth whoami` printed the minted API key itself")
	}
}

// AC61: 19 rows, 18 quotable.
//
// Two witnesses, not one. The JSON answer is FERRY's; the text rendering is the
// CLI's own, and comparing the id sets between them in **both directions** is
// what catches a renderer that drops or invents a row. Counting only the JSON
// would check FERRY and nothing about the CLI; counting only the text would
// check the parser against itself.
func TestCorridorsListAnswersNineteenRowsEighteenQuotable(t *testing.T) {
	c := newCLI(t)
	c.login()

	doc, res := c.json("corridors", "list")
	if res.exitCode != 0 {
		t.Fatalf("corridors list exited %d\nstderr:\n%s", res.exitCode, res.stderr)
	}

	corridors := parseCorridorsJSON(t, doc.Response)

	if len(corridors) != 19 {
		t.Errorf("FERRY answered %d corridors, AC61 says 19", len(corridors))
	}

	quotable := 0
	for _, corridor := range corridors {
		if corridor.Quotable {
			quotable++
		}
	}

	if quotable != 18 {
		t.Errorf("%d of %d corridors are quotable, AC61 says 18 of 19", quotable, len(corridors))
	}

	text := c.run("corridors", "list")
	if text.exitCode != 0 {
		t.Fatalf("corridors list in text mode exited %d\nstderr:\n%s", text.exitCode, text.stderr)
	}

	fromJSON := make([]string, 0, len(corridors))
	for _, corridor := range corridors {
		fromJSON = append(fromJSON, corridor.ID)
	}

	assertSetsEqual(t, "corridor ids, JSON answer against text rendering",
		fromJSON, parseCorridorIDsFromText(t, text.stdout))
}

// The floor for the test above, as its own example.
//
// If `parseCorridorIDsFromText` stops matching — the renderer changes its
// labels, say — every comparison over its output becomes vacuously true, and
// "the text rendering agrees with the JSON" would pass with the text rendering
// contributing nothing. This is the assertion that the reader found anything at
// all, and it is separate so that "the reader broke" and "the content changed"
// are two different failures.
func TestTheCorridorTextReaderFindsRows(t *testing.T) {
	c := newCLI(t)
	c.login()

	text := c.run("corridors", "list")
	if text.exitCode != 0 {
		t.Fatalf("corridors list in text mode exited %d\nstderr:\n%s", text.exitCode, text.stderr)
	}

	ids := parseCorridorIDsFromText(t, text.stdout)
	if len(ids) == 0 {
		t.Fatalf("the text reader found no corridor ids in %d bytes of output; every comparison "+
			"over it would be vacuous.\noutput:\n%s", len(text.stdout), text.stdout)
	}
}

type corridorRow struct {
	ID       string `json:"id"`
	Quotable bool   `json:"quotable"`
}

func parseCorridorsJSON(t *testing.T, raw json.RawMessage) []corridorRow {
	t.Helper()

	var body struct {
		Data []corridorRow `json:"data"`
	}

	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("parse the corridor collection: %v\n%s", err, raw)
	}

	if len(body.Data) == 0 {
		t.Fatalf("the corridor collection has an empty `data`; every count below would be about nothing")
	}

	return body.Data
}

// parseCorridorIDsFromText reads the ids out of `writeCorridors`'s output,
// which labels each one `id:` at the start of a line.
func parseCorridorIDsFromText(t *testing.T, out string) []string {
	t.Helper()

	var ids []string

	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "id:")
		if !ok {
			continue
		}

		if id := strings.TrimSpace(rest); id != "" {
			ids = append(ids, id)
		}
	}

	return ids
}

// AC61's fifth clause: `transfers create` prices and moves nothing, and the
// plan token is on stdout.
func TestTransfersCreatePricesAndPrintsThePlanTokenOnce(t *testing.T) {
	c := newCLI(t)
	c.login()
	c.mintSandboxKey()

	tr := newTransfer(t)

	doc, res := c.json(append([]string{"transfers", "create"}, tr.flags()...)...)
	if res.exitCode != 0 {
		t.Fatalf("transfers create exited %d, want 0\nstdout:\n%s\nstderr:\n%s",
			res.exitCode, res.stdout, res.stderr)
	}

	if doc.HTTP == nil || doc.HTTP.Status != 201 {
		t.Fatalf("simulate did not answer 201: http=%+v\nerror: %+v", doc.HTTP, doc.Error)
	}

	if doc.Outcome.Class != "done" {
		t.Errorf("outcome.class is %q, want done", doc.Outcome.Class)
	}

	// The whole of "moves nothing": the decision table's money verdict for a
	// simulate that priced is `no`, and it is what a caller reads before
	// deciding whether anything needs reconciling.
	if doc.Outcome.Money != "no" {
		t.Errorf("outcome.money is %q after a simulate, want no — a priced plan moved money", doc.Outcome.Money)
	}

	tokens := planTokenPattern.FindAllString(res.stdout, -1)
	if len(tokens) != 1 {
		t.Fatalf("stdout carries %d plan tokens, want exactly 1 (AC61: printed once)\nstdout:\n%s",
			len(tokens), res.stdout)
	}

	// And the run it minted must not be holding the token afterwards: a
	// simulate-only run stores no plan token (AC46, M45).
	show := c.run("runs", "show", runID(t, doc))
	if found := planTokenPattern.FindAllString(show.stdout, -1); len(found) != 0 {
		t.Errorf("`runs show` prints %d plan token(s); a simulate-only run stores none\n%s",
			len(found), show.stdout)
	}
}

// AC61's sixth and seventh clauses: `--broadcast --yes` executes, `status` is
// rendered, and `runs show` has the token scrubbed.
func TestBroadcastExecutesAndRunsShowHasTheTokenScrubbed(t *testing.T) {
	c := newCLI(t)
	c.login()
	c.mintSandboxKey()

	tr := newTransfer(t)

	doc, res := c.json(append([]string{"transfers", "create", "--broadcast", "--yes"}, tr.flags()...)...)
	if res.exitCode != 0 {
		t.Fatalf("transfers create --broadcast exited %d, want 0\nstdout:\n%s\nstderr:\n%s",
			res.exitCode, res.stdout, res.stderr)
	}

	if doc.HTTP == nil || doc.HTTP.Status != 201 {
		t.Fatalf("execute did not answer 201: http=%+v\nerror: %+v", doc.HTTP, doc.Error)
	}

	var transaction struct {
		Object    string `json:"object"`
		Status    string `json:"status"`
		SubStatus string `json:"subStatus"`
	}

	if err := json.Unmarshal(doc.Response, &transaction); err != nil {
		t.Fatalf("parse the transaction: %v\n%s", err, doc.Response)
	}

	// "`status` rendered", asserted as non-empty rather than as a literal.
	// The fixture answers `processing`/`processing.fundsPulled` today; pinning
	// those strings would make this test about the fixture's choice of status
	// rather than about the CLI carrying whatever status it was given.
	if transaction.Status == "" {
		t.Errorf("the execute 201 carried no `status`, which is what AC61 requires rendered")
	}

	if !strings.Contains(res.stdout, transaction.Status) {
		t.Errorf("the document does not carry the status %q it reported", transaction.Status)
	}

	run := runID(t, doc)

	// The token was minted by this invocation's simulate and used by its
	// execute. Once the run is terminal the ledger scrubs it (AC13), so
	// neither rendering may carry it.
	for _, mode := range [][]string{{"runs", "show", run}, {"runs", "show", run, "--output", "json"}} {
		out := c.run(mode...)
		if found := planTokenPattern.FindAllString(out.stdout, -1); len(found) != 0 {
			t.Errorf("`%s` prints %d plan token(s); a terminal run's token is scrubbed\n%s",
				strings.Join(mode, " "), len(found), out.stdout)
		}
	}
}
