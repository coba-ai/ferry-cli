package outcome_test

// The decision table, held to the 72 recorded interactions.
//
// == Why this file exists, and what makes it different from coverage_test.go
//
// `coverage_test.go` holds the table to `docs/api/errors.md` and
// `docs/api/openapi.yaml`. Those are documents: they say what the API answers.
// `testdata/recorded/` is 72 scenarios captured from the real Rails app by
// driving the Rack stack, byte-stable and re-derived in CI. It says what the
// API *did* answer.
//
// The distinction is not academic here. This project's API documentation has
// been materially wrong more than once — the transfer decision table, three
// status codes, a scope list, and a `retriable` flag on `DESTINATION_TOO_NEW`
// that would have had an agent retry a permanent refusal forever (PLAN §1.4).
// The table was written from those documents, before any recording existed. So
// every example below draws its expectation from a recording or from the
// manifest, never from the table, and never from `errors.md` where a recording
// can answer instead.
//
// == The two directions, and the floors
//
// Both directions are asserted, as separate examples, because a subset check
// in the convenient direction is the defect this repository keeps shipping
// (`docs/dev-loop-learnings.md`, "A subset assertion in one direction cannot
// see an absence").
//
//   - Forward: every (status, code) pair the recordings contain has a row, and
//     the row's verdict agrees with what the recording shows.
//   - Backward: every row is witnessed by a recording, or is on one of three
//     accounted-for lists — `errors.md`'s "Codes you will not see", the
//     webhook-only set `openapi.yaml` declares, and `notRecorded` below, whose
//     entries carry a reason naming the fact the scenario set does not
//     produce. Set equality, so the list can neither grow quietly nor go
//     stale.
//
// Every reader raises on input it cannot parse rather than skipping it, and
// every example asserts a floor on what it found. A derivation that silently
// stopped recognising anything would otherwise compare two empty sets and pass
// (`docs/dev-loop-learnings.md`, "An auditor that skips the inputs it cannot
// read reports a clean set over a subset").

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/outcome"
	"gopkg.in/yaml.v3"
)

// What the recording set contained when this file was written. They are floors
// and not equalities: U0 may add a scenario under an amendment, and that must
// not redden here. A reader that stops matching drops *below* them, which is
// the direction this guards.
const (
	floorRecordings  = 72
	floorAnswers     = 79
	floorPairs       = 31
	floorCodes       = 31
	floorCommandBody = 9
)

const recordedDir = "testdata/recorded"

// Rows whose code no recording contains, each with the fact the scenario set
// of PLAN §5.10 does not produce. This is the backward direction's exclusion
// list, and it is asserted as part of an equality: a code that gains a
// recording must leave this list, and a row that loses its recording must join
// it.
//
// "Unwitnessed" is not "unreachable". Everything here is emitted by the live
// app; what is missing is a scenario that provokes it. The three genuinely
// unreachable codes are not here — they come from `errors.md` itself.
var notRecorded = map[string]string{
	"CORRIDOR_NOT_EXECUTABLE":    "A 422 from the quote translator for a corridor that is published but not executable. §5.10 records `CORRIDOR_UNSUPPORTED`, `AMOUNT_INVALID` and `QUOTE_REJECTED` as the quote surface's representatives and stops there.",
	"DESTINATION_NOT_EXECUTABLE": "Same quote surface, same representative set. Provoking it needs a destination instrument the corridor publishes and refuses, which no fixture builds.",
	"INSTRUMENT_INVALID":         "Same quote surface, same representative set. Needs an instrument field outside the arm's vocabulary, which `simulate.amount_invalid.422` covers the shape of.",
	"CURSOR_INVALID":             "Needs a cursor this API did not issue. `keys.list.page2.200` records the pagination path with a cursor the API handed out; no scenario forges one.",
	"ENVIRONMENT_DISABLED":       "Needs an environment row in a disabled state. The recorder provisions sandbox and live in their ordinary states and never disables one.",
	"ORGANIZATION_SUSPENDED":     "Needs a suspended organization. Same reason: the recorder provisions one unsuspended organization and does not change its status.",
	"TOKEN_EXPIRED":              "Needs a token past its `expires_at`. `auth.token_revoked.401` records the other half of the same 401 pair — a credential that authenticated once and no longer does.",
	"FORBIDDEN":                  "The generic authorisation refusal. §5.10 records the specific ones instead — `WRONG_TOKEN_CLASS`, `INSUFFICIENT_SCOPE`, `STEP_UP_REQUIRED` — because those are the ones a CLI caller can act on differently.",
	"ROLE_FORBIDDEN":             "Needs a personal access token whose member role is below the operation's. The recorder issues one PAT, at a role that may perform every operation it drives.",
	"IDEMPOTENCY_KEY_INVALID":    "Needs an `Idempotency-Key` outside 1–255 bytes of printable ASCII. `simulate.key_required.400` records the absent-key half of the same check; the malformed-key half has no scenario.",
	"MALFORMED_JSON":             "Needs a request body that is not JSON at all. `auth.unsupported_media_type.415` records the adjacent refusal — a body sent under the wrong content type — and no scenario sends broken JSON under the right one.",
}

// operationIDs maps `openapi.yaml`'s `operationId` to this package's
// `Operation`. The two vocabularies differ deliberately: the money operations
// carry the server's `operation_id` spelling because AC74 selects the terminal
// rule by the command body's `operation` field. The map is a duplicate list,
// which is the point — TestRecordedRoutesCoverEveryOperation holds it to both
// sides rather than deriving one from the other.
var operationIDs = map[string]outcome.Operation{
	"getMe":            outcome.OpGetMe,
	"listApiKeys":      outcome.OpListAPIKeys,
	"createApiKey":     outcome.OpCreateAPIKey,
	"getApiKey":        outcome.OpGetAPIKey,
	"revokeApiKey":     outcome.OpRevokeAPIKey,
	"listCorridors":    outcome.OpListCorridors,
	"getCorridor":      outcome.OpGetCorridor,
	"simulateTransfer": outcome.OpSimulateTransfer,
	"executeTransfer":  outcome.OpExecuteTransfer,
	"getCommand":       outcome.OpGetCommand,
}

// The two routes no CLI operation reaches, excluded by name so a third one
// appearing in the contract is a parse failure rather than a silent omission.
var nonCLIPaths = map[string]string{
	"/oms/webhooks/{locator}": "FERRY's webhook ingress; this CLI never posts to it.",
	"/v1/{path}":              "the catch-all that answers 404 for an unrouted /v1 path.",
}

// --- the recording set -------------------------------------------------

// recordedAnswer is one HTTP answer out of one recorded interaction, reduced
// to what the table reads plus the facts this file asserts against.
type recordedAnswer struct {
	scenario string
	index    int
	op       outcome.Operation
	status   int

	// envelope is non-nil when the body is the seven-key error envelope.
	envelope *recordedEnvelope
	// command is non-nil when the body is a command (`object: "command"`),
	// whether it arrived as a 202 or as a poll's 200.
	command *recordedCommand
	// transactionStatus is the execute 201 body's `status`. Empty when the
	// body is not a transaction.
	transactionStatus string

	headers map[string]string

	// constructed is the manifest's `constructed` note, non-empty for the
	// scenarios U0 could not reach from the request path and seeded with
	// the ledger builders instead. See requireLiveEvidence.
	constructed string
}

// requireLiveEvidence refuses to read a response header as evidence about
// what the live request path did, when the manifest says the scenario was
// built rather than driven.
//
// This is the trap the `constructed` note exists to prevent, and it is an easy
// one to walk into: `execute.contract_violation.502` carries
// `Idempotency-Replayed: true` and a `Ferry-Command-Id`, which would read as
// "a command row exists, so T1 ran and the plan was consumed" — and §1.2.5
// says the opposite, that `#validate_quote` answers without touching the plan.
// Both are true, because the row in that recording was seeded by U0 and
// replayed. A constructed answer is authoritative for the *shape* FERRY
// renders and says nothing about when FERRY decides.
func requireLiveEvidence(t *testing.T, a recordedAnswer, claim string) bool {
	t.Helper()

	if isLiveEvidence(a) {
		return true
	}

	t.Errorf("%s is a constructed recording (%q), so its headers cannot establish %s. Either the claim needs a scenario driven from the request path, or this example must stop reading it",
		a.scenario, a.constructed, claim)

	return false
}

// isLiveEvidence is the predicate, separated so it can be shown to say "no"
// about something. Every scenario the examples below read headers from is
// live today, so the guard never fires in an ordinary run — and a guard that
// has never been observed to refuse anything is indistinguishable from one
// that cannot. TestTheLiveEvidenceGuardRefusesAConstructedRecording is what
// makes the difference visible.
func isLiveEvidence(a recordedAnswer) bool { return a.constructed == "" }

type recordedEnvelope struct {
	code      string
	retriable bool
	details   map[string]any
}

type recordedCommand struct {
	operation     string
	state         string
	contradiction bool
	commandID     string
	transactionID string
	// resultPresent is `result != null`; resultStatus is `result.body.status`.
	resultPresent bool
	resultStatus  string
	lastErrorCode string
}

// input is the tuple C4 names, assembled from the recording and from nothing
// else.
func (a recordedAnswer) input() outcome.Input {
	in := outcome.Input{
		Op:        a.op,
		Status:    a.status,
		CommandID: a.headers["Ferry-Command-Id"],
		Replayed:  a.headers["Idempotency-Replayed"] == "true",
	}

	if a.envelope != nil {
		in.Envelope = &outcome.Envelope{Code: a.envelope.code, Details: a.envelope.details}
	}

	if a.status >= 200 && a.status < 300 {
		in.Success = &outcome.Success{Parsed: true, TransactionStatus: a.transactionStatus}
	}

	return in
}

func (a recordedAnswer) where() string {
	return fmt.Sprintf("%s[%d]", a.scenario, a.index)
}

// --- the readers -------------------------------------------------------

type manifestFile struct {
	Scenarios []struct {
		Scenario string `json:"scenario"`
		SHA256   string `json:"sha256"`
		// Constructed is U0's note that the row behind this answer was
		// seeded with the ledger builders because the scripted gateway
		// cannot reach it from the request path (§5.10).
		Constructed string `json:"constructed"`
		Expect      []struct {
			Status     int               `json:"status"`
			Code       *string           `json:"code"`
			BodyEquals map[string]string `json:"body_equals"`
		} `json:"expect"`
	} `json:"scenarios"`
	Unreachable []struct {
		Scenario string `json:"scenario"`
		Reason   string `json:"reason"`
	} `json:"unreachable"`
}

func readManifest(t *testing.T) manifestFile {
	t.Helper()

	var m manifestFile
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(recordedDir, "MANIFEST.json"))), &m); err != nil {
		t.Fatalf("parsing MANIFEST.json: %v", err)
	}

	if len(m.Scenarios) < floorRecordings {
		t.Fatalf("MANIFEST.json lists %d scenarios; the reader expects at least %d and has stopped matching",
			len(m.Scenarios), floorRecordings)
	}

	if len(m.Unreachable) == 0 {
		t.Fatal("MANIFEST.json has an empty `unreachable` list; the reader has stopped matching")
	}

	return m
}

type recordingFile struct {
	Scenario     string `json:"scenario"`
	Interactions []struct {
		Request struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"request"`
		Response struct {
			Status  int               `json:"status"`
			Headers map[string]string `json:"headers"`
			Body    json.RawMessage   `json:"body"`
		} `json:"response"`
	} `json:"interactions"`
}

// readRecordedAnswers reads every file under `testdata/recorded/` and
// returns one entry per HTTP answer.
//
// It raises on anything it cannot place: a file the manifest does not list, a
// request no contract route matches, a body that is neither the error envelope
// nor a 2xx document. A reader that dropped those would report a clean result
// over whatever it happened to understand.
func readRecordedAnswers(t *testing.T) []recordedAnswer {
	t.Helper()

	manifest := readManifest(t)
	listed := map[string]bool{}
	constructed := map[string]string{}

	for _, s := range manifest.Scenarios {
		listed[s.Scenario] = true
		constructed[s.Scenario] = s.Constructed
	}

	routes := readContractRoutes(t)
	envelopeKeys := readEnvelopeKeys(t)

	dir := filepath.Join(repoRoot(t), recordedDir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", recordedDir, err)
	}

	var (
		answers []recordedAnswer
		files   int
	)

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || name == "MANIFEST.json" {
			continue
		}

		files++

		var rec recordingFile
		if err := json.Unmarshal([]byte(readFile(t, filepath.Join(recordedDir, name))), &rec); err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		if rec.Scenario+".json" != name {
			t.Fatalf("%s names the scenario %q", name, rec.Scenario)
		}

		if !listed[rec.Scenario] {
			t.Fatalf("%s is on disk but MANIFEST.json does not list it", name)
		}

		for i, it := range rec.Interactions {
			a := readAnswer(t, rec.Scenario, i,
				it.Request.Method, it.Request.Path,
				it.Response.Status, it.Response.Headers, it.Response.Body,
				routes, envelopeKeys)
			a.constructed = constructed[rec.Scenario]

			answers = append(answers, a)
		}
	}

	if files < floorRecordings {
		t.Fatalf("read %d recordings out of %s; the reader expects at least %d and has stopped matching",
			files, recordedDir, floorRecordings)
	}

	if files != len(manifest.Scenarios) {
		t.Fatalf("%s holds %d recordings and MANIFEST.json lists %d", recordedDir, files, len(manifest.Scenarios))
	}

	if len(answers) < floorAnswers {
		t.Fatalf("read %d answers out of %d recordings; the reader expects at least %d and has stopped matching",
			len(answers), files, floorAnswers)
	}

	return answers
}

func readAnswer(
	t *testing.T,
	scenario string, index int,
	method, path string,
	status int, headers map[string]string, body json.RawMessage,
	routes []contractRoute, envelopeKeys []string,
) recordedAnswer {
	t.Helper()

	a := recordedAnswer{
		scenario: scenario,
		index:    index,
		op:       resolveOperation(t, scenario, index, method, path, routes),
		status:   status,
		headers:  headers,
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		t.Fatalf("%s[%d]: the response body is not a JSON object, which this reader cannot place: %v", scenario, index, err)
	}

	if raw, ok := doc["error"]; ok {
		a.envelope = readRecordedEnvelope(t, scenario, index, raw, envelopeKeys)

		if status >= 200 && status < 300 {
			t.Fatalf("%s[%d]: a %d carries the error envelope", scenario, index, status)
		}

		return a
	}

	if status < 200 || status >= 300 {
		t.Fatalf("%s[%d]: a %d whose body is not the error envelope. AC83 says the table classifies it `pending` on money and `transient` on a read; this reader cannot tell which row it belongs to, so it refuses rather than guessing",
			scenario, index, status)
	}

	switch object, _ := doc["object"].(string); object {
	case "command":
		a.command = readRecordedCommand(t, scenario, index, doc)
	case "transaction":
		a.transactionStatus, _ = doc["status"].(string)
	case "simulation", "principal", "api_key", "corridor", "list":
		// A 2xx the table classifies by status alone.
	default:
		t.Fatalf("%s[%d]: a 2xx body whose `object` is %q, which this reader does not know", scenario, index, object)
	}

	return a
}

func readRecordedEnvelope(t *testing.T, scenario string, index int, raw any, envelopeKeys []string) *recordedEnvelope {
	t.Helper()

	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s[%d]: `error` is not an object", scenario, index)
	}

	// AC17's shape, held to `openapi.yaml`'s own `required` list. This is
	// what makes "is this the error envelope" a contract question rather
	// than this reader's opinion — and it is the premise every example
	// below rests on when it treats a non-envelope body as AC83's row.
	var present []string
	for k := range obj {
		present = append(present, k)
	}

	assertSetsEqual(t, fmt.Sprintf("%s[%d] envelope keys", scenario, index),
		present, envelopeKeys, "the recorded envelope", "openapi.yaml's Error.error.required")

	code, ok := obj["code"].(string)
	if !ok || code == "" {
		t.Fatalf("%s[%d]: the envelope has no `code`", scenario, index)
	}

	retriable, ok := obj["retriable"].(bool)
	if !ok {
		t.Fatalf("%s[%d]: the envelope's `retriable` is %v, not a boolean", scenario, index, obj["retriable"])
	}

	details, _ := obj["details"].(map[string]any)

	return &recordedEnvelope{code: code, retriable: retriable, details: details}
}

func readRecordedCommand(t *testing.T, scenario string, index int, doc map[string]any) *recordedCommand {
	t.Helper()

	c := &recordedCommand{}
	c.operation, _ = doc["operation"].(string)
	c.state, _ = doc["state"].(string)
	c.commandID, _ = doc["id"].(string)
	c.transactionID, _ = doc["transaction_id"].(string)
	_, c.contradiction = doc["contradiction"].(map[string]any)

	if result, ok := doc["result"].(map[string]any); ok {
		c.resultPresent = true
		if rbody, ok := result["body"].(map[string]any); ok {
			c.resultStatus, _ = rbody["status"].(string)
		}
	}

	if lastError, ok := doc["last_error"].(map[string]any); ok {
		c.lastErrorCode, _ = lastError["code"].(string)
	}

	if c.state == "" || c.operation == "" {
		t.Fatalf("%s[%d]: a command body with no `state` or no `operation`", scenario, index)
	}

	return c
}

func (c *recordedCommand) input() outcome.CommandInput {
	in := outcome.CommandInput{
		Operation:     c.operation,
		State:         c.state,
		Contradiction: c.contradiction,
		CommandID:     c.commandID,
		TransactionID: c.transactionID,
		LastErrorCode: c.lastErrorCode,
	}

	if c.resultPresent {
		in.Result = &outcome.CommandResult{Status: 201, TransactionStatus: c.resultStatus}
	}

	return in
}

// --- the contract readers ----------------------------------------------

type contractRoute struct {
	method   string
	segments []string
	op       outcome.Operation
}

// readContractRoutes turns `openapi.yaml`'s paths into matchable routes. The
// operation a recorded request belongs to is decided by the contract, not by
// the scenario's name — a name is a label U0 chose and a path is what the app
// answered.
func readContractRoutes(t *testing.T) []contractRoute {
	t.Helper()

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(readFile(t, "contract/openapi.yaml")), &doc); err != nil {
		t.Fatalf("parsing openapi.yaml: %v", err)
	}

	paths := mappingValue(t, root(t, &doc), "paths")

	var (
		routes   []contractRoute
		excluded []string
	)

	for i := 0; i+1 < len(paths.Content); i += 2 {
		path := paths.Content[i].Value

		if _, skip := nonCLIPaths[path]; skip {
			excluded = append(excluded, path)

			continue
		}

		methods := paths.Content[i+1]
		for j := 0; j+1 < len(methods.Content); j += 2 {
			method := strings.ToUpper(methods.Content[j].Value)
			id := mappingValue(t, methods.Content[j+1], "operationId").Value

			op, known := operationIDs[id]
			if !known {
				t.Fatalf("openapi.yaml declares %s %s as %q, which this file does not map to an operation", method, path, id)
			}

			routes = append(routes, contractRoute{method: method, segments: strings.Split(path, "/"), op: op})
		}
	}

	assertSetsEqual(t, "paths outside this CLI's surface",
		excluded, mapKeys(nonCLIPaths), "openapi.yaml", "this file's nonCLIPaths")

	if len(routes) != len(operationIDs) {
		t.Fatalf("openapi.yaml declares %d CLI routes for %d operation ids", len(routes), len(operationIDs))
	}

	return routes
}

// readEnvelopeKeys is `Error.error.required` — the seven keys AC17 requires.
func readEnvelopeKeys(t *testing.T) []string {
	t.Helper()

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(readFile(t, "contract/openapi.yaml")), &doc); err != nil {
		t.Fatalf("parsing openapi.yaml: %v", err)
	}

	schemas := mappingValue(t, mappingValue(t, root(t, &doc), "components"), "schemas")
	errSchema := mappingValue(t, schemas, "Error")
	inner := mappingValue(t, mappingValue(t, errSchema, "properties"), "error")

	var required []string
	for _, n := range mappingValue(t, inner, "required").Content {
		required = append(required, n.Value)
	}

	if len(required) != 7 {
		t.Fatalf("openapi.yaml's Error.error.required names %d keys; AC17 says seven", len(required))
	}

	return required
}

// readCommandStates is `Command.state`'s enum.
func readCommandStates(t *testing.T) []string {
	t.Helper()

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(readFile(t, "contract/openapi.yaml")), &doc); err != nil {
		t.Fatalf("parsing openapi.yaml: %v", err)
	}

	schemas := mappingValue(t, mappingValue(t, root(t, &doc), "components"), "schemas")
	state := mappingValue(t, mappingValue(t, mappingValue(t, schemas, "Command"), "properties"), "state")

	var states []string
	for _, n := range mappingValue(t, state, "enum").Content {
		states = append(states, n.Value)
	}

	if len(states) == 0 {
		t.Fatal("read no states out of openapi.yaml's Command.state enum; the reader has stopped matching")
	}

	return states
}

func resolveOperation(t *testing.T, scenario string, index int, method, path string, routes []contractRoute) outcome.Operation {
	t.Helper()

	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}

	got := strings.Split(path, "/")

	var matched []contractRoute

	for _, r := range routes {
		if r.method != strings.ToUpper(method) || len(r.segments) != len(got) {
			continue
		}

		ok := true

		for k, want := range r.segments {
			if strings.HasPrefix(want, "{") {
				continue
			}

			if want != got[k] {
				ok = false

				break
			}
		}

		if ok {
			matched = append(matched, r)
		}
	}

	if len(matched) != 1 {
		t.Fatalf("%s[%d]: %s %s matches %d contract routes; this reader will not guess which operation answered",
			scenario, index, method, path, len(matched))
	}

	return matched[0].op
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// --- the examples ------------------------------------------------------

// The route derivation is itself held to both sides, so an operation that
// stops being reachable from a recorded path cannot quietly shrink what every
// example below covers.
func TestRecordedRoutesCoverEveryOperation(t *testing.T) {
	routes := readContractRoutes(t)

	var fromContract []string
	for _, r := range routes {
		fromContract = append(fromContract, string(r.op))
	}

	var declared []string
	for _, op := range outcome.Operations {
		declared = append(declared, string(op))
	}

	assertSetsEqual(t, "operations", fromContract, declared,
		"openapi.yaml's operationIds through this file's map", "outcome.Operations")

	// And every operation is actually exercised by a recording, so the map
	// above cannot name a route the scenario set never drives.
	exercised := map[outcome.Operation]bool{}
	for _, a := range readRecordedAnswers(t) {
		exercised[a.op] = true
	}

	var seen []string
	for op := range exercised {
		seen = append(seen, string(op))
	}

	assertSetsEqual(t, "operations exercised by a recording", seen, declared,
		"the recordings", "outcome.Operations")
}

// Direction one. Every (status, code) pair the recordings contain resolves to
// a declared row — not to the unknown-code fallback, which `Lookup` is what
// makes distinguishable.
//
// A recorded answer the table has no row for is the serious case: the fallback
// produces a perfectly ordinary `pending` Outcome, so nothing at runtime would
// say the table had never heard of it.
func TestEveryRecordedAnswerHasARow(t *testing.T) {
	answers := readRecordedAnswers(t)

	pairs := map[string]bool{}
	codes := map[string]bool{}
	classes := map[outcome.OpClass]bool{}

	var missing []string

	for _, a := range answers {
		class, ok := outcome.ClassOf(a.op)
		if !ok {
			t.Fatalf("%s: the recording's operation %q has no class", a.where(), a.op)
		}

		classes[class] = true

		if a.envelope == nil {
			continue
		}

		pairs[fmt.Sprintf("%d %s", a.status, a.envelope.code)] = true
		codes[a.envelope.code] = true

		if _, _, found := outcome.Lookup(a.envelope.code); !found {
			missing = append(missing, fmt.Sprintf("%s: %d %s", a.where(), a.status, a.envelope.code))
		}
	}

	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("the recordings contain %d answer(s) the table has no row for:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}

	if len(pairs) < floorPairs || len(codes) < floorCodes {
		t.Fatalf("extracted %d (status, code) pair(s) over %d code(s) from %d answers; expected at least %d and %d. The extraction has stopped matching, and an empty comparison would pass",
			len(pairs), len(codes), len(answers), floorPairs, floorCodes)
	}

	// The pairs must span every operation class, or "covered" means covered
	// for reads.
	var seen []string
	for c := range classes {
		seen = append(seen, string(c))
	}

	var declared []string
	for _, c := range outcome.OpClasses {
		declared = append(declared, string(c))
	}

	assertSetsEqual(t, "operation classes the recordings exercise", seen, declared,
		"the recordings", "outcome.OpClasses")
}

// Direction two, and the one a subset check cannot make: every row is
// witnessed or accounted for, as an equality.
//
// A row for a code the API cannot emit is dead vocabulary — harmless on its
// own, and the thing that makes a census stop meaning anything, because the
// count no longer says what was checked against reality.
func TestEveryRowIsWitnessedOrAccountedFor(t *testing.T) {
	answers := readRecordedAnswers(t)
	manifest := readManifest(t)
	catalogue := readCatalogue(t)

	accounted := map[string]string{}

	witnessed := 0

	for _, a := range answers {
		if a.envelope == nil {
			continue
		}

		if _, seen := accounted[a.envelope.code]; !seen {
			witnessed++
		}

		accounted[a.envelope.code] = "witnessed by " + a.scenario
	}

	// A104/A300: `execute.policy_denied.403` declares its code as the
	// pattern `/^POLICY_/` rather than as one literal, because the denial
	// the recorder provokes is one of a family the ledger treats
	// identically. The pattern witnesses the family, so each member it
	// matches is accounted for by that scenario and not by `notRecorded`.
	byPattern := 0

	for _, s := range manifest.Scenarios {
		for _, e := range s.Expect {
			if e.Code == nil || !strings.HasPrefix(*e.Code, "/") {
				continue
			}

			re := compileManifestPattern(t, s.Scenario, *e.Code)

			matched := 0

			for code := range catalogue {
				if !re.MatchString(code) {
					continue
				}

				matched++

				if _, seen := accounted[code]; !seen {
					byPattern++
					accounted[code] = fmt.Sprintf("matched by %s's %s", s.Scenario, *e.Code)
				}
			}

			if matched == 0 {
				t.Errorf("%s declares the code pattern %s, which matches no catalogued code", s.Scenario, *e.Code)
			}
		}
	}

	for _, code := range readNeverEmitted(t) {
		if scenario, seen := accounted[code]; seen && strings.HasPrefix(scenario, "witnessed") {
			t.Errorf(`%s is under errors.md's "Codes you will not see" and a recording contains it (%s)`, code, scenario)
		}

		accounted[code] = "never emitted, per errors.md"
	}

	for _, code := range readWebhookOnlyCodes(t, catalogue) {
		accounted[code] = "emitted only by the webhook ingress, per openapi.yaml"
	}

	for code, reason := range notRecorded {
		if scenario, seen := accounted[code]; seen {
			t.Errorf("notRecorded names %s, which is already accounted for: %s. Remove the entry; the reason it carries has gone stale", code, scenario)
		}

		if len(reason) < 40 {
			t.Errorf("notRecorded's reason for %s is %d characters. An exclusion list whose reasons are one sentence repeated is a subset check with extra steps", code, len(reason))
		}

		accounted[code] = "not recorded: " + reason
	}

	assertSetsEqual(t, "table rows", outcome.Codes(), mapKeys(accounted),
		"the decision table", "the recordings plus the accounted-for lists")

	// The floor. Most of a 59-row census being accounted for by an
	// exclusion list would satisfy the equality above and establish
	// nothing.
	if witnessed < floorCodes {
		t.Fatalf("only %d of %d rows are witnessed by a recording; expected at least %d",
			witnessed, len(outcome.Codes()), floorCodes)
	}

	t.Logf("%d rows: %d witnessed by a recording, %d by the manifest's code patterns, %d accounted for otherwise",
		len(outcome.Codes()), witnessed, byPattern, len(accounted)-witnessed-byPattern)
}

func compileManifestPattern(t *testing.T, scenario, declared string) *regexp.Regexp {
	t.Helper()

	if len(declared) < 2 || !strings.HasPrefix(declared, "/") || !strings.HasSuffix(declared, "/") {
		t.Fatalf("%s declares the code %q, which this reader cannot read as a pattern", scenario, declared)
	}

	re, err := regexp.Compile(strings.TrimSuffix(strings.TrimPrefix(declared, "/"), "/"))
	if err != nil {
		t.Fatalf("%s declares the code pattern %q, which does not compile: %v", scenario, declared, err)
	}

	return re
}

// Which recorded answers are evidence about the live request path, pinned.
//
// Three scenarios were seeded with the ledger builders and replayed, because
// the scripted gateway cannot drive the app into those rows. Their bodies and
// headers are what FERRY renders for such a row and are authoritative for
// that; they are not evidence about *when* FERRY decides, which is the
// question `Ferry-Command-Id` is read for elsewhere in this file.
//
// The set is held to an equality, in both directions, for two reasons. A
// scenario that becomes constructed silently would turn a live observation
// into a seeded one under the same name — and the examples reading its
// headers would stay green while their premise had gone. A scenario that
// stops being constructed is a claim that got stronger, and `requireLiveEvidence`
// should start admitting it.
func TestTheConstructedRecordingsArePinned(t *testing.T) {
	manifest := readManifest(t)

	got := map[string]string{}

	for _, s := range manifest.Scenarios {
		if s.Constructed == "" {
			continue
		}

		got[s.Scenario] = s.Constructed

		if len(s.Constructed) < 40 {
			t.Errorf("%s is marked constructed with a %d-character note; the note is the only thing that says what the recording may be read for",
				s.Scenario, len(s.Constructed))
		}
	}

	assertSetsEqual(t, "constructed recordings", mapKeys(got),
		[]string{
			// A `failed_terminal` row seeded and replayed: the live
			// `#validate_quote` refusal (§1.2.5) touches no plan and
			// would carry neither header.
			"execute.contract_violation.502",
			// Same route; §5.10 names this one explicitly.
			"execute.upstream_unavailable.503",
			// Its third interaction reads a row moved to `completed`.
			"simulate.202.then_completed",
		},
		"MANIFEST.json", "the set this file pins")
}

// The positive control for the guard above: it must actually refuse a
// constructed recording, and admit a live one.
//
// Asserted against real answers out of the recording set rather than against
// a hand-built struct, because the thing that could break is the manifest's
// `constructed` note failing to reach `recordedAnswer` — which a synthetic
// fixture would never notice.
func TestTheLiveEvidenceGuardRefusesAConstructedRecording(t *testing.T) {
	answers := readRecordedAnswers(t)

	var refused, admitted int

	for _, a := range answers {
		switch a.scenario {
		case "execute.contract_violation.502", "execute.upstream_unavailable.503":
			if isLiveEvidence(a) {
				t.Errorf("%s is marked constructed in MANIFEST.json and the guard admits it; the note is not reaching the answer",
					a.where())
			} else {
				refused++
			}

		case "execute.execution_suspended.403", "execute.policy_denied.403":
			if !isLiveEvidence(a) {
				t.Errorf("%s is driven from the request path and the guard refuses it", a.where())
			} else {
				admitted++
			}
		}
	}

	if refused < 2 || admitted < 2 {
		t.Fatalf("the guard refused %d constructed answers and admitted %d live ones; expected at least 2 of each, or it has not been shown to do either",
			refused, admitted)
	}
}

// The safety-critical cross-check, and the one with an authority independent
// of every document: the `retriable` byte the app itself put on the wire.
//
// `openapi.yaml` defines it as "whether the byte-identical request could ever
// succeed". `Outcome.SameKeySafe` answers "may the caller resend the
// byte-identical request under the same Idempotency-Key". That is the same
// question, so on every recorded answer the two must agree — and a
// disagreement on the money path is either the CLI telling a caller to resend
// something the server will never accept, or telling them to mint a new key
// for a request the server would have deduplicated. The second one sends the
// transfer twice.
func TestRecordedRetriableAgreesWithSameKeySafety(t *testing.T) {
	answers := readRecordedAnswers(t)
	catalogue := readCatalogue(t)

	var (
		checked  int
		retryYes int
		money    int
	)

	for _, a := range answers {
		if a.envelope == nil {
			continue
		}

		checked++

		if a.envelope.retriable {
			retryYes++
		}

		class, _ := outcome.ClassOf(a.op)
		if class.IsMoney() {
			money++
		}

		got := outcome.Classify(a.input())

		if got.SameKeySafe != a.envelope.retriable {
			t.Errorf("%s (%s, %d %s): the table says same_key_safe=%v via %q; the recorded answer says retriable=%v",
				a.where(), a.op, a.status, a.envelope.code, got.SameKeySafe, got.Class, a.envelope.retriable)
		}

		// And the recording against the catalogue — two authorities,
		// neither of them the table. A recording that drifted from
		// `errors.md` would otherwise make the assertion above agree with
		// the wrong thing.
		entry, known := catalogue[a.envelope.code]
		if !known {
			t.Errorf("%s: the recorded code %q is not in errors.md", a.where(), a.envelope.code)

			continue
		}

		if entry.retriable != a.envelope.retriable {
			t.Errorf("%s: the recorded answer says retriable=%v for %s; errors.md says %v",
				a.where(), a.envelope.retriable, a.envelope.code, entry.retriable)
		}

		if entry.status != a.status {
			t.Errorf("%s: the app answered %s with %d; errors.md says %d",
				a.where(), a.envelope.code, a.status, entry.status)
		}
	}

	if checked < floorPairs || money == 0 || retryYes == 0 || retryYes == checked {
		t.Fatalf("checked %d recorded error answers (%d on a money operation, %d retriable); the comparison needs at least %d spanning both values of `retriable` and at least one money operation, or it is vacuous",
			checked, money, retryYes, floorPairs)
	}
}

// AC83 and A311, at every status the app actually answered with.
//
// `pending` is the class that means the money may have moved. A body that did
// not come out of FERRY's error renderer says nothing about whether FERRY
// executed, so on a money send the only honest class is `pending`/6 — never a
// `refused`, whose entire meaning is "nothing was sent".
//
// The statuses come from the recordings rather than from a list, plus the
// three PLAN §5.6 names as having no row at all.
func TestANonEnvelopeBodyIsPendingOnMoneyAtEveryRecordedStatus(t *testing.T) {
	answers := readRecordedAnswers(t)

	statuses := map[int]bool{
		// §5.6: "any status with no row (`413`, `415`, HTML `502`, …)".
		// 415 does have a catalogued code, and `auth.unsupported_media_type.415`
		// records it carrying the envelope — but a middlebox answering 415
		// with its own body is the case this row is for.
		413: true,
		415: true,
		502: true,
	}

	ops := map[outcome.Operation]bool{}

	for _, a := range answers {
		statuses[a.status] = true
		ops[a.op] = true
	}

	for _, op := range mapKeys(operationsByName(ops)) {
		operation := outcome.Operation(op)

		class, ok := outcome.ClassOf(operation)
		if !ok {
			t.Fatalf("%s has no operation class", op)
		}

		for _, status := range sortedInts(statuses) {
			// Envelope and Success both nil: the answer arrived and its
			// body is not something this CLI can read.
			got := outcome.Classify(outcome.Input{Op: operation, Status: status})

			wantClass, wantExit := outcome.ClassTransient, 5
			if class.IsMoney() {
				wantClass, wantExit = outcome.ClassPending, 6
			}

			if got.Class != wantClass || got.Exit != wantExit {
				t.Errorf("%s (%s) answered %d with a body that is not the envelope: got %q/%d, want %q/%d",
					op, class, status, got.Class, got.Exit, wantClass, wantExit)
			}
		}
	}

	if len(statuses) < 15 || len(ops) < len(outcome.Operations) {
		t.Fatalf("exercised %d statuses over %d operations; expected at least 15 statuses and all %d operations",
			len(statuses), len(ops), len(outcome.Operations))
	}
}

// C19/B2/AC71, driven by both recordings rather than by one.
//
// A300 added `body_equals` to the manifest for exactly this: the code and the
// field are the same in both scenarios, and only the *value* of
// `details.reason` separates a client bug from a transfer that may already
// have executed under a credential this caller no longer holds. So the
// expectation is read out of the manifest, the input out of the recording, and
// the two are held to each other first.
func TestBothKeyReuseRecordingsSplitOnTheirRecordedReason(t *testing.T) {
	answers := readRecordedAnswers(t)
	manifest := readManifest(t)

	declared := map[string]string{}

	for _, s := range manifest.Scenarios {
		for _, e := range s.Expect {
			if reason, ok := e.BodyEquals["error.details.reason"]; ok {
				declared[s.Scenario] = reason
			}
		}
	}

	// What each reason means for the caller, from AC71 and PLAN §5.6. Not
	// from the table: these two rows are the split the table exists to make.
	want := map[string]struct {
		class outcome.Class
		exit  int
	}{
		"different_request":    {outcome.ClassRefusedFix, 3},
		"different_credential": {outcome.ClassEscalate, 7},
	}

	seen := map[string]bool{}
	exits := map[int]bool{}

	for _, a := range answers {
		if a.envelope == nil || a.envelope.code != "IDEMPOTENCY_KEY_REUSED" {
			continue
		}

		reason, _ := a.envelope.details["reason"].(string)

		if got, ok := declared[a.scenario]; !ok {
			t.Errorf("%s records a reuse but declares no `body_equals` for `error.details.reason`", a.scenario)
		} else if got != reason {
			t.Errorf("%s: the manifest declares reason %q and the recording carries %q", a.scenario, got, reason)
		}

		w, known := want[reason]
		if !known {
			t.Errorf("%s: recorded reuse reason %q, which AC71 does not name", a.where(), reason)

			continue
		}

		seen[reason] = true

		got := outcome.Classify(a.input())
		if got.Class != w.class || got.Exit != w.exit {
			t.Errorf("%s (reason %q): got %q/%d, want %q/%d",
				a.where(), reason, got.Class, got.Exit, w.class, w.exit)
		}

		exits[got.Exit] = true
	}

	if len(seen) != len(want) {
		t.Fatalf("found recordings for %d of the %d reuse reasons AC71 names: %v", len(seen), len(want), mapKeys(seen))
	}

	if len(exits) != 2 {
		t.Errorf("both reuse recordings exit %v; the split C19 exists to make has collapsed", sortedInts(exits))
	}
}

// A304's other half, and CS-7's. `UPSTREAM_BUSY` is one code for two facts,
// and the recordings hold both: `execute.upstream_busy.no_command.503` is the
// quote re-read failing before T1, with the plan untouched and nothing to
// poll, and `execute.upstream_busy.with_command.503` is recovery giving up
// after the plan was consumed, with a `failed_retriable` command of FERRY's
// own to poll.
//
// The tell is the `Ferry-Command-Id` header, and the recordings differ in
// exactly that. The expectation is AC20's: without it 5, with it 6.
func TestUpstreamBusySplitsAsTheTwoRecordingsDiffer(t *testing.T) {
	answers := readRecordedAnswers(t)

	var withID, withoutID []recordedAnswer

	for _, a := range answers {
		if a.envelope == nil || a.envelope.code != "UPSTREAM_BUSY" {
			continue
		}

		if a.headers["Ferry-Command-Id"] != "" {
			withID = append(withID, a)
		} else {
			withoutID = append(withoutID, a)
		}
	}

	if len(withID) == 0 || len(withoutID) == 0 {
		t.Fatalf("the recordings hold %d UPSTREAM_BUSY answers with a Ferry-Command-Id and %d without; the split needs both",
			len(withID), len(withoutID))
	}

	for _, a := range append(append([]recordedAnswer{}, withID...), withoutID...) {
		requireLiveEvidence(t, a, "which of the two UPSTREAM_BUSY paths answered")
	}

	for _, a := range withID {
		got := outcome.Classify(a.input())
		if got.Class != outcome.ClassPending || got.Exit != 6 {
			t.Errorf("%s carries Ferry-Command-Id, so FERRY has a command of its own and has scheduled recovery: got %q/%d, want pending/6",
				a.where(), got.Class, got.Exit)
		}

		if !strings.Contains(got.Next, a.headers["Ferry-Command-Id"]) {
			t.Errorf("%s: `next` does not name the command to poll:\n  %s", a.where(), got.Next)
		}
	}

	for _, a := range withoutID {
		got := outcome.Classify(a.input())
		if got.Class != outcome.ClassTransient || got.Exit != 5 {
			t.Errorf("%s carries no Ferry-Command-Id, so the quote read failed before T1 and the plan is untouched: got %q/%d, want transient/5",
				a.where(), got.Class, got.Exit)
		}
	}
}

// A304. `EXECUTION_SUSPENDED` is one code that means two different things, and
// the operation is what separates them: on simulate no plan exists to spend,
// on execute the refusal is decided in T1, after the quote re-read, with the
// plan already consumed.
//
// Both are recorded, and the recordings carry the evidence for the split
// rather than only the code: the execute answer carries a `Ferry-Command-Id`,
// so a command row exists under the key, and a command row is what T1 writes.
// The simulate answer carries none.
func TestExecutionSuspendedSplitsByOperationAsTheRecordingsShow(t *testing.T) {
	answers := readRecordedAnswers(t)

	byClass := map[outcome.OpClass]recordedAnswer{}

	for _, a := range answers {
		if a.envelope == nil || a.envelope.code != "EXECUTION_SUSPENDED" {
			continue
		}

		class, _ := outcome.ClassOf(a.op)
		byClass[class] = a
	}

	execute, okExec := byClass[outcome.OpMoneyExecute]
	simulate, okSim := byClass[outcome.OpMoneySimulate]

	if !okExec || !okSim {
		t.Fatalf("EXECUTION_SUSPENDED is recorded on %d of the two money operations; the split needs both", len(byClass))
	}

	requireLiveEvidence(t, execute, "that T1 writes a command row")
	requireLiveEvidence(t, simulate, "that a simulate refusal writes none")

	if execute.headers["Ferry-Command-Id"] == "" {
		t.Errorf("%s carries no Ferry-Command-Id. A304 says this refusal is decided in T1, which writes a command row; if the app no longer does, the execute column's `refused_resimulate` is no longer evidenced",
			execute.where())
	}

	if simulate.headers["Ferry-Command-Id"] != "" {
		t.Errorf("%s carries a Ferry-Command-Id, so a command row exists and `refused_fix`'s \"nothing was spent\" is no longer evidenced", simulate.where())
	}

	if got := outcome.Classify(execute.input()); got.Class != outcome.ClassRefusedResimulate || got.Exit != 4 {
		t.Errorf("%s: got %q/%d, want refused_resimulate/4 — the plan was consumed at T1", execute.where(), got.Class, got.Exit)
	}

	if got := outcome.Classify(simulate.input()); got.Class != outcome.ClassRefusedFix || got.Exit != 3 {
		t.Errorf("%s: got %q/%d, want refused_fix/3 — no plan exists to spend", simulate.where(), got.Class, got.Exit)
	}
}

// The plan refusals, and the one of them the recordings distinguish.
//
// `Refused` rolls the plan consumption back and `Denied` commits it, and the
// observable difference is whether a command row survives to set
// `Ferry-Command-Id`. Every recorded `PLAN_*` answer carries none, which is
// what `begin.rb:47-51` says and what the table's `planUnusable` shape
// assumes. All of them are 4 regardless, because the remedy is a new plan
// either way — but the header is the fact, so it is asserted rather than
// assumed.
func TestRecordedPlanRefusalsAreAllResimulate(t *testing.T) {
	answers := readRecordedAnswers(t)

	seen := map[string]bool{}

	for _, a := range answers {
		if a.envelope == nil || !strings.HasPrefix(a.envelope.code, "PLAN_") {
			continue
		}

		class, _ := outcome.ClassOf(a.op)
		if !class.IsMoney() {
			continue
		}

		seen[a.envelope.code] = true

		if !requireLiveEvidence(t, a, "that `Refused` leaves no command row") {
			continue
		}

		if id := a.headers["Ferry-Command-Id"]; id != "" {
			t.Errorf("%s carries Ferry-Command-Id %s. A plan refusal is `Refused`, which rolls the consumption back and leaves no command row; if one now survives, the plan may be spent and the row's sentence says it is not",
				a.where(), id)
		}

		got := outcome.Classify(a.input())
		if got.Class != outcome.ClassRefusedResimulate || got.Exit != 4 {
			t.Errorf("%s (%s): got %q/%d, want refused_resimulate/4", a.where(), a.envelope.code, got.Class, got.Exit)
		}
	}

	if len(seen) < 5 {
		t.Fatalf("found %d distinct PLAN_* codes in the recordings: %v; expected the five §5.10 records", len(seen), mapKeys(seen))
	}
}

// The spend policy, and the half of §1.2.6 a recording can see.
//
// `Ledger::Begin` consumes the plan before it reads the policy. `Refused`
// rolls that back and `Denied` commits a `failed_terminal` row with the plan
// gone — so a denial is a pre-send refusal that still costs the plan, and the
// difference from a `PLAN_*` refusal is visible on the wire as the
// `Ferry-Command-Id` of the row `Denied` committed.
//
// `execute.policy_denied.403` carries one, and the manifest declares its code
// as the pattern `/^POLICY_/` because the family is decided identically.
func TestTheRecordedPolicyDenialSpendsThePlan(t *testing.T) {
	answers := readRecordedAnswers(t)

	found := 0

	for _, a := range answers {
		if a.envelope == nil || !strings.HasPrefix(a.envelope.code, "POLICY_") {
			continue
		}

		class, _ := outcome.ClassOf(a.op)
		if class != outcome.OpMoneyExecute {
			continue
		}

		found++

		if requireLiveEvidence(t, a, "that `Denied` commits a row") && a.headers["Ferry-Command-Id"] == "" {
			t.Errorf("%s carries no Ferry-Command-Id. A policy denial is `Denied`, which commits a failed_terminal row with the plan consumed; with no row the refusal may be a `Refused` and the plan may still be usable",
				a.where())
		}

		got := outcome.Classify(a.input())
		if got.Class != outcome.ClassRefusedResimulate || got.Exit != 4 {
			t.Errorf("%s (%s): got %q/%d, want refused_resimulate/4 — the plan is spent, so a new key on the same plan would be refused forever",
				a.where(), a.envelope.code, got.Class, got.Exit)
		}

		if got.SameKeySafe {
			t.Errorf("%s: a spent plan cannot be re-executed, so the same key is not safe", a.where())
		}
	}

	if found == 0 {
		t.Fatal("no recorded POLICY_* answer on an execute; §5.10 records `execute.policy_denied.403`")
	}
}

// The band under everything else: no answer FERRY refused may come out of the
// table as a success.
//
// This is the cheapest possible check and the most expensive failure — a
// refusal reported as exit 0 is a transfer a caller believes was accepted.
// It is asserted over every recorded error answer rather than per family, so
// a code whose row nothing else here names is still covered.
func TestNoRecordedRefusalClassifiesAsSuccess(t *testing.T) {
	answers := readRecordedAnswers(t)

	checked := 0

	for _, a := range answers {
		if a.envelope == nil {
			continue
		}

		checked++

		got := outcome.Classify(a.input())

		if got.Exit == 0 {
			t.Errorf("%s (%d %s) classifies %q/0 — FERRY refused this request",
				a.where(), a.status, a.envelope.code, got.Class)
		}

		if got.Money == outcome.MoneyAcceptedUpstream {
			t.Errorf("%s (%d %s) reports money=accepted_upstream; no transaction came back with this answer",
				a.where(), a.status, a.envelope.code)
		}

		if got.Class == "" {
			t.Errorf("%s: the table produced an empty class", a.where())
		}
	}

	if checked < floorPairs {
		t.Fatalf("checked %d recorded error answers; expected at least %d", checked, floorPairs)
	}
}

// AC53: `409 IDEMPOTENCY_KEY_IN_PROGRESS` means poll, never resend under a new
// key. Both money operations record it, and both carry the command to poll in
// `details.command_id` as well as in the header.
func TestKeyInProgressIsPendingAndNamesTheCommand(t *testing.T) {
	answers := readRecordedAnswers(t)

	found := 0

	for _, a := range answers {
		if a.envelope == nil || a.envelope.code != "IDEMPOTENCY_KEY_IN_PROGRESS" {
			continue
		}

		found++

		got := outcome.Classify(a.input())
		if got.Class != outcome.ClassPending || got.Exit != 6 {
			t.Errorf("%s: got %q/%d, want pending/6", a.where(), got.Class, got.Exit)
		}

		id, _ := a.envelope.details["command_id"].(string)
		if id == "" {
			t.Errorf("%s: the recorded answer carries no `details.command_id`, so there is nothing to poll", a.where())

			continue
		}

		if !strings.Contains(got.Next, id) {
			t.Errorf("%s: `next` does not name the command to poll:\n  %s", a.where(), got.Next)
		}
	}

	if found < 2 {
		t.Fatalf("found %d IDEMPOTENCY_KEY_IN_PROGRESS recordings; §5.10 records one on each money operation", found)
	}
}

// AC22 and C9, over the recorded 2xx answers.
//
// The execute 201 is the row where a wrong verdict is a transfer reported as
// failed that was accepted, or the reverse, and both recordings exist:
// `execute.201.processing` and `execute.201.failed_status` differ in the
// body's `status` and in nothing else that the table reads.
func TestRecordedSuccessesClassifyByWhatTheBodyCarries(t *testing.T) {
	answers := readRecordedAnswers(t)

	var executes, simulates, reads, accepted int

	for _, a := range answers {
		if a.status < 200 || a.status >= 300 {
			continue
		}

		class, _ := outcome.ClassOf(a.op)
		got := outcome.Classify(a.input())

		switch {
		case a.status == 202:
			accepted++

			if got.Class != outcome.ClassPending || got.Exit != 6 {
				t.Errorf("%s: a 202 is %q/%d, want pending/6", a.where(), got.Class, got.Exit)
			}

		case a.op == outcome.OpExecuteTransfer:
			executes++

			wantClass, wantExit, wantMoney := outcome.ClassAcceptedUpstream, 0, outcome.MoneyAcceptedUpstream
			if a.transactionStatus == "failed" {
				wantClass, wantExit, wantMoney = outcome.ClassUpstreamFailed, 8, outcome.MoneySeeUpstream
			}

			if got.Class != wantClass || got.Exit != wantExit || got.Money != wantMoney {
				t.Errorf("%s: the recorded transaction is status=%q; got %q/%d money=%q, want %q/%d money=%q",
					a.where(), a.transactionStatus, got.Class, got.Exit, got.Money, wantClass, wantExit, wantMoney)
			}

		case a.op == outcome.OpSimulateTransfer:
			simulates++

			if got.Class != outcome.ClassDone || got.Exit != 0 {
				t.Errorf("%s: a simulate 201 is %q/%d, want done/0", a.where(), got.Class, got.Exit)
			}

			// §1.2.9: the replayed answer's stored body has no
			// `plan.token`, so the caller must be told the token is not
			// in this answer.
			if a.headers["Idempotency-Replayed"] == "true" && len(got.Warnings) == 0 {
				t.Errorf("%s is a replayed simulate and carries no warning that it holds no plan.token", a.where())
			}

		case !class.IsMoney():
			reads++

			if got.Class != outcome.ClassDone || got.Exit != 0 {
				t.Errorf("%s: a read 2xx is %q/%d, want done/0", a.where(), got.Class, got.Exit)
			}
		}
	}

	if executes < 2 || simulates < 2 || accepted < 3 || reads < 10 {
		t.Fatalf("classified %d execute 201s, %d simulate 201s, %d 202s and %d read 2xxs; expected at least 2, 2, 3 and 10",
			executes, simulates, accepted, reads)
	}
}

// AC74, AC21 and AC51, over every command body the recordings contain —
// whether it arrived as a 202, as a poll's 200, or as `commands get`.
//
// Each expectation comes from the body's own fields and from the rule PLAN
// §5.6's terminal table states for them, so a body that starts saying
// something different reddens here.
func TestRecordedCommandBodiesClassifyAsTheirFieldsRequire(t *testing.T) {
	answers := readRecordedAnswers(t)
	states := readCommandStates(t)

	known := map[string]bool{}
	for _, s := range states {
		known[s] = true
	}

	var (
		bodies        int
		seenStates    = map[string]bool{}
		contradictory int
	)

	for _, a := range answers {
		if a.command == nil {
			continue
		}

		bodies++
		seenStates[a.command.state] = true

		if !known[a.command.state] {
			t.Errorf("%s: the recorded state %q is not in openapi.yaml's Command.state enum", a.where(), a.command.state)
		}

		if !outcome.KnownState(a.command.state) {
			t.Errorf("%s: this package does not know the recorded state %q", a.where(), a.command.state)
		}

		got := outcome.ClassifyCommand(a.command.input())

		switch {
		case a.command.contradiction:
			// `openapi.yaml`, Command.contradiction: "Stop and escalate
			// whatever `state` says — including `failed_terminal`".
			contradictory++

			assertCommandClass(t, a, got, outcome.ClassEscalate, 7,
				"the body carries a contradiction")

		case a.command.state == "needs_operator":
			assertCommandClass(t, a, got, outcome.ClassEscalate, 7,
				"an operator holds this command")

		case !outcome.IsTerminalState(a.command.state):
			assertCommandClass(t, a, got, outcome.ClassPending, 6,
				"the state is not terminal, so the outcome is not established")

		case a.command.state == "completed" && a.command.operation == string(outcome.OpSimulateTransfer):
			// B4/AC74: the stored quote body has no `plan.token`, so the
			// plan exists and is unusable.
			assertCommandClass(t, a, got, outcome.ClassRefusedResimulate, 4,
				"a simulate that completed after the request ended issued no plan token")

		case a.command.state == "completed" && a.command.operation == string(outcome.OpExecuteTransfer):
			wantClass, wantExit := outcome.ClassAcceptedUpstream, 0
			if a.command.resultStatus == "failed" {
				wantClass, wantExit = outcome.ClassUpstreamFailed, 8
			}

			assertCommandClass(t, a, got, wantClass, wantExit,
				fmt.Sprintf("a completed execute whose stored transaction is status=%q", a.command.resultStatus))

		case a.command.state == "failed_terminal":
			// Whatever the code says, a terminal command will not move
			// again, so no class may tell the caller to come back.
			if got.Class == outcome.ClassPending || got.Class == outcome.ClassTransient {
				t.Errorf("%s: a failed_terminal command classifies %q/%d, which tells the caller to come back to something that will never change",
					a.where(), got.Class, got.Exit)
			}
		}
	}

	if bodies < floorCommandBody || len(seenStates) < 6 || contradictory < 2 {
		t.Fatalf("read %d command bodies over %d states with %d contradictions; expected at least %d, 6 and 2",
			bodies, len(seenStates), contradictory, floorCommandBody)
	}

	// Both terminal states must be among them, or the terminal rule — the
	// half of §5.6 that decides what a polled transfer finally means — is
	// covered by nothing here.
	for _, s := range outcome.TerminalStates {
		if !seenStates[s] {
			t.Errorf("no recorded command body is in the terminal state %q", s)
		}
	}
}

func assertCommandClass(t *testing.T, a recordedAnswer, got outcome.Outcome, class outcome.Class, exit int, because string) {
	t.Helper()

	if got.Class != class || got.Exit != exit {
		t.Errorf("%s: %s, so it must be %q/%d; got %q/%d", a.where(), because, class, exit, got.Class, got.Exit)
	}
}

// A command's `last_error.code` is not drawn from the API error catalogue.
//
// The recordings carry `UPSTREAM_UNKNOWN`, which `errors.md` does not name at
// all, alongside fields — `category`, `body_class`, `rejection_shaped`,
// `transport_error` — that no wire envelope has. So `last_error` is the
// recovery layer's classification of what the upstream did, and it merely
// overlaps the wire vocabulary in places.
//
// `ClassifyCommand` reads it through the wire table anyway. That is safe for
// the values recorded here and it is pinned as a set, so a third value
// arriving — including one that collides with a catalogue code meaning
// something else — reddens rather than being classified by coincidence.
func TestRecordedCommandLastErrorVocabulary(t *testing.T) {
	answers := readRecordedAnswers(t)
	catalogue := readCatalogue(t)

	seen := map[string]bool{}

	for _, a := range answers {
		if a.command == nil || a.command.lastErrorCode == "" {
			continue
		}

		seen[a.command.lastErrorCode] = true
	}

	assertSetsEqual(t, "recorded last_error codes", mapKeys(seen),
		[]string{"UPSTREAM_UNAVAILABLE", "UPSTREAM_UNKNOWN"},
		"the recordings", "the set this file pins")

	// The half that makes the point: one of the two is not a wire code.
	if _, ok := catalogue["UPSTREAM_UNKNOWN"]; ok {
		t.Error("errors.md now names UPSTREAM_UNKNOWN; the two vocabularies have converged and this example's premise is stale")
	}

	if _, ok := catalogue["UPSTREAM_UNAVAILABLE"]; !ok {
		t.Error("errors.md no longer names UPSTREAM_UNAVAILABLE; the overlap this example pins has gone")
	}
}

func sortedInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Ints(out)

	return out
}

func operationsByName(ops map[outcome.Operation]bool) map[string]bool {
	out := map[string]bool{}
	for op := range ops {
		out[string(op)] = true
	}

	return out
}
