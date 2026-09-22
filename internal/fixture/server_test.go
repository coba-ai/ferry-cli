package fixture_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/fixture"
)

// These are U3's criteria: AC29 (the loader's two-directional check and the
// 599), AC30 (structural matching), AC31 (sequences and the request log),
// AC32 (no invented headers), AC77 (every expectation axis enforced,
// `body_equals` included) and AC91 (holding a request open).
//
// `harness.Run` is not parallel-safe (A314) and nothing here uses it; the
// server is, so the tests that drive one say so.

// ---------------------------------------------------------------------------
// AC29 — the loader, both directions, with a floor under the comparison
// ---------------------------------------------------------------------------

func TestLoadAcceptsTheCommittedRecordings(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)

	if got, want := len(set.Order), len(set.Manifest.Scenarios); got != want {
		t.Fatalf("loaded %d recordings for %d manifest entries", got, want)
	}

	// The floor. Every comparison in Load is an equality between two sets,
	// and an equality between two empty sets is green for every corruption
	// it exists to catch. These numbers are lower bounds on the committed
	// directory (72 scenarios, 79 interactions at `54cccd1`), not a copy of
	// it: a scenario added is fine, a directory that quietly stopped being
	// found is not.
	const (
		floorScenarios    = 60
		floorInteractions = 70
	)

	if len(set.Order) < floorScenarios {
		t.Errorf("only %d scenarios loaded; fewer than %d means the derivation stopped finding them", len(set.Order), floorScenarios)
	}

	checks := set.Checks

	if checks.Files != len(set.Order) {
		t.Errorf("read %d files for %d scenarios", checks.Files, len(set.Order))
	}

	if checks.Digests != len(set.Order) {
		t.Errorf("compared %d digests for %d scenarios (AC29)", checks.Digests, len(set.Order))
	}

	if checks.Interactions < floorInteractions {
		t.Errorf("checked %d interactions, floor is %d", checks.Interactions, floorInteractions)
	}

	// Per axis, so that an axis whose enforcement was deleted cannot hide
	// behind the axes that remain.
	for _, tally := range []struct {
		axis  string
		got   int
		floor int
	}{
		{"status", checks.Status, floorInteractions},
		{"code (literal)", checks.CodeLiteral, 20},
		{"code (pattern)", checks.CodePattern, 1},
		{"code (absent)", checks.CodeAbsent, 20},
		{"headers_present", checks.HeadersPresent, 20},
		{"headers_absent", checks.HeadersAbsent, 10},
		{"body_has", checks.BodyHas, 100},
		{"body_lacks", checks.BodyLacks, 10},
		{"body_equals", checks.BodyEquals, 2},
	} {
		if tally.got < tally.floor {
			t.Errorf("the loader made %d %s assertions; floor is %d", tally.got, tally.axis, tally.floor)
		}
	}

	if checks.Status != checks.Interactions {
		t.Errorf("status was asserted %d times for %d interactions", checks.Status, checks.Interactions)
	}

	if got := checks.CodeLiteral + checks.CodePattern + checks.CodeAbsent; got != checks.Interactions {
		t.Errorf("code was asserted %d times for %d interactions", got, checks.Interactions)
	}

	// A sequence must be among them or AC31 has nothing to replay and A301
	// has nothing to decode.
	sequences := 0

	for _, name := range set.Order {
		if len(set.Recordings[name].Interactions) > 1 {
			sequences++
		}
	}

	if sequences < 1 {
		t.Errorf("no multi-interaction scenario loaded; `expect` as an array (A301) is untested")
	}

	if len(set.Manifest.Unreachable) < 1 {
		t.Errorf("the manifest declares nothing unreachable; §5.10 names three")
	}
}

func TestLoadRefusesAFileTheManifestDoesNotName(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)

	raw, err := os.ReadFile(filepath.Join(dir, "me.api_key.200.json"))
	if err != nil {
		t.Fatalf("reading a recording to copy: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "me.api_key.200.copy.json"), raw, 0o600); err != nil {
		t.Fatalf("writing the unnamed recording: %v", err)
	}

	refuses(t, dir, "me.api_key.200.copy.json is on disk and the manifest does not name it")
}

func TestLoadRefusesAManifestEntryWithNoFile(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)

	if err := os.Remove(filepath.Join(dir, "execute.policy_denied.403.json")); err != nil {
		t.Fatalf("removing a recording: %v", err)
	}

	refuses(t, dir, "the manifest names execute.policy_denied.403 and there is no execute.policy_denied.403.json")
}

// The floor, as a control rather than as an assertion about a number: with
// both sides emptied, every set equality in Load holds. A loader that passes
// here is a loader that would pass over a directory that had lost everything.
func TestLoadRefusesADirectoryWithNothingInIt(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the copy: %v", err)
	}

	for _, entry := range entries {
		if entry.Name() == fixture.ManifestName {
			continue
		}

		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatalf("emptying the copy: %v", err)
		}
	}

	editManifest(t, dir, func(manifest map[string]any) {
		manifest["scenarios"] = []any{}
	})

	refuses(t, dir, "no recordings on disk", "the manifest names no scenarios")
}

func TestLoadRefusesARecordingThatChanged(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)
	path := filepath.Join(dir, "commands.get.completed.200.json")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}

	// One byte of whitespace: no decoded value changes, and the digest does.
	if err := os.WriteFile(path, append(raw, ' '), 0o600); err != nil {
		t.Fatalf("writing the recording: %v", err)
	}

	refuses(t, dir, "commands.get.completed.200: sha256 is", "the manifest records")
}

func TestLoadRefusesAnUnreachableScenarioThatHasAFile(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)

	editManifest(t, dir, func(manifest map[string]any) {
		unreachable, _ := manifest["unreachable"].([]any)
		manifest["unreachable"] = append(unreachable, map[string]any{
			"scenario": "me.pat.200",
			"reason":   "claimed unreachable while its recording sits on disk",
		})
	})

	refuses(t, dir,
		"me.pat.200 is both a recorded scenario and unreachable",
		"me.pat.200 is declared unreachable and me.pat.200.json exists")
}

func TestLoadRefusesAnUnreachableScenarioWithNoReason(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)

	editManifest(t, dir, func(manifest map[string]any) {
		unreachable, ok := manifest["unreachable"].([]any)
		if !ok || len(unreachable) == 0 {
			t.Fatalf("the manifest declares nothing unreachable")
		}

		unreachable[0].(map[string]any)["reason"] = "   "
	})

	refuses(t, dir, "is declared unreachable with no reason")
}

// ---------------------------------------------------------------------------
// AC77 — every expectation axis, enforced, `body_equals` included
// ---------------------------------------------------------------------------

// Each row breaks exactly one axis of one real expectation and names the axis
// the refusal must cite. The corruption is written to both the manifest and
// the recording, with the digest refreshed, so the only thing left wrong is
// the expectation against the response beneath it — otherwise every row would
// go red on "the file and the manifest differ" and none of them would be
// about its axis.
func TestLoadEnforcesEveryExpectationAxis(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		scenario string
		index    int
		edit     func(expect map[string]any)
		want     string
	}{
		{
			name:     "status",
			scenario: "me.api_key.200",
			edit:     func(expect map[string]any) { expect["status"] = 201 },
			want:     "status: expected 201, recorded 200",
		},
		{
			name:     "code, literal",
			scenario: "execute.plan_expired.410",
			edit:     func(expect map[string]any) { expect["code"] = "PLAN_NOT_FOUND" },
			want:     `code: expected "PLAN_NOT_FOUND", recorded "PLAN_EXPIRED"`,
		},
		{
			name:     "code, pattern",
			scenario: "execute.policy_denied.403",
			edit:     func(expect map[string]any) { expect["code"] = "/^UPSTREAM_/" },
			want:     `code: expected a match for "/^UPSTREAM_/"`,
		},
		{
			name:     "code, declared absent",
			scenario: "execute.plan_expired.410",
			edit:     func(expect map[string]any) { expect["code"] = nil },
			want:     "code: expected no error code, recorded PLAN_EXPIRED",
		},
		{
			name:     "code, declared present on a body that has none",
			scenario: "me.api_key.200",
			edit:     func(expect map[string]any) { expect["code"] = "TOKEN_INVALID" },
			want:     "recorded body has no error.code",
		},
		{
			name:     "headers_present",
			scenario: "me.api_key.200",
			edit: func(expect map[string]any) {
				expect["headers_present"] = append(expect["headers_present"].([]any), "Retry-After")
			},
			want: "headers_present: Retry-After is not in the recorded response",
		},
		{
			name:     "headers_absent",
			scenario: "me.api_key.200",
			edit: func(expect map[string]any) {
				expect["headers_absent"] = append(expect["headers_absent"].([]any), "Ferry-Environment")
			},
			want: "headers_absent: Ferry-Environment is in the recorded response",
		},
		{
			name:     "body_has",
			scenario: "me.api_key.200",
			edit: func(expect map[string]any) {
				expect["body_has"] = append(expect["body_has"].([]any), "credential.secret")
			},
			want: "body_has: credential.secret is absent from the recorded body",
		},
		{
			name:     "body_has, present but null",
			scenario: "commands.get.reserved.200",
			edit: func(expect map[string]any) {
				expect["body_has"] = append(expect["body_has"].([]any), "contradiction")
			},
			want: "body_has: contradiction is null in the recorded body",
		},
		{
			name:     "body_lacks",
			scenario: "me.api_key.200",
			edit: func(expect map[string]any) {
				expect["body_lacks"] = append(expect["body_lacks"].([]any), "object")
			},
			want: "body_lacks: object is present in the recorded body",
		},
		{
			name:     "body_equals, the value A300 exists for",
			scenario: "execute.key_reused.different_credential.409",
			edit: func(expect map[string]any) {
				expect["body_equals"] = map[string]any{"error.details.reason": "different_request"}
			},
			want: `body_equals: error.details.reason is "different_credential", expected "different_request"`,
		},
		{
			name:     "body_equals, a path the body does not carry",
			scenario: "execute.key_reused.different_credential.409",
			edit: func(expect map[string]any) {
				expect["body_equals"] = map[string]any{"error.details.nope": "x"}
			},
			want: "body_equals: error.details.nope is absent from the recorded body",
		},
		{
			name:     "an axis in a later entry of a sequence",
			scenario: "execute.202.then_completed",
			index:    2,
			edit:     func(expect map[string]any) { expect["status"] = 202 },
			want:     "execute.202.then_completed[2]: status: expected 202, recorded 200",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			dir := copyRecordings(t)
			editExpect(t, dir, row.scenario, row.index, row.edit)

			_, err := fixture.Load(dir)
			if err == nil {
				t.Fatalf("Load accepted a directory whose %s expectation is false", row.name)
			}

			if !strings.Contains(err.Error(), row.want) {
				t.Fatalf("refused for the wrong reason.\nwant substring: %s\ngot: %v", row.want, err)
			}

			// The corruption is in the expectation, not in the digest and
			// not in the two copies disagreeing. If the refusal cites
			// either, this row is not the control it claims to be.
			rejects(t, err, "sha256 is", "expect differ")
		})
	}
}

// A300's case, stated as the property rather than as a corruption: the two
// reuse scenarios are identical on every presence axis, and only `body_equals`
// tells them apart. If a loader drops the axis, it serves either recording
// under either name.
func TestBodyEqualsIsTheOnlyAxisThatSeparatesTheTwoReuseScenarios(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)

	credential := manifestExpect(t, set, "execute.key_reused.different_credential.409")
	request := manifestExpect(t, set, "execute.key_reused.different_request.409")

	if credential.Status != request.Status {
		t.Fatalf("the two reuse scenarios differ on status (%d, %d); the axis is not load-bearing", credential.Status, request.Status)
	}

	if !reflect.DeepEqual(credential.Code, request.Code) {
		t.Fatalf("the two reuse scenarios differ on code; the axis is not load-bearing")
	}

	if !reflect.DeepEqual(sorted(credential.BodyHas), sorted(request.BodyHas)) {
		t.Fatalf("the two reuse scenarios differ on body_has; the axis is not load-bearing")
	}

	if reflect.DeepEqual(credential.BodyEquals, request.BodyEquals) {
		t.Fatalf("the two reuse scenarios have the same body_equals; nothing separates them")
	}

	if got := credential.BodyEquals["error.details.reason"]; got != "different_credential" {
		t.Errorf("different_credential asserts reason %v", got)
	}

	if got := request.BodyEquals["error.details.reason"]; got != "different_request" {
		t.Errorf("different_request asserts reason %v", got)
	}
}

func TestLoadRefusesAnExpectationAxisItDoesNotDecode(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)

	editExpect(t, dir, "me.api_key.200", 0, func(expect map[string]any) {
		expect["body_matches"] = map[string]any{"object": "^principal$"}
	})

	refuses(t, dir, "body_matches", "not decoded by this loader")
}

func TestLoadRefusesAnExpectationMissingAnAxis(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)

	editExpect(t, dir, "execute.key_reused.different_credential.409", 0, func(expect map[string]any) {
		delete(expect, "body_equals")
	})

	refuses(t, dir, "missing axes body_equals")
}

func TestLoadRefusesOneExpectationForThreeInteractions(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)
	dropExpectEntry(t, dir, "execute.202.then_completed", 1)

	refuses(t, dir, "2 expectations for 3 interactions")
}

func TestLoadRefusesWhenTheFileAndTheManifestDisagree(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)

	editManifest(t, dir, func(manifest map[string]any) {
		entry := manifestEntry(t, manifest, "me.api_key.200")
		expect := entry["expect"].([]any)[0].(map[string]any)
		expect["body_has"] = append(expect["body_has"].([]any), "credential.token_last4")
	})

	refuses(t, dir, "me.api_key.200: the file's expect and the manifest's expect differ")
}

func TestLoadRefusesAnExpectationWeakenedToItsStatus(t *testing.T) {
	t.Parallel()

	dir := copyRecordings(t)

	editExpect(t, dir, "execute.policy_denied.403", 0, func(expect map[string]any) {
		expect["code"] = nil
		expect["headers_present"] = []any{}
		expect["headers_absent"] = []any{}
		expect["body_has"] = []any{}
		expect["body_lacks"] = []any{}
		expect["body_equals"] = map[string]any{}
	})

	refuses(t, dir, "asserts nothing but its status")
}

// ---------------------------------------------------------------------------
// The recordings against a second authority
// ---------------------------------------------------------------------------

// Every request in the directory is a request the contract declares, and
// every `Idempotency-Key` sits on a route the contract gives the parameter
// to (AC16's column).
//
// The loader cannot check a recorded *request* against anything — `expect` is
// a vocabulary about answers — so a corrupted request body or path would make
// the fixture answer something FERRY never would. `api.Routes` is pinned to
// `openapi.yaml` by AC25 and is an authority U0 did not write, which is what
// makes this a comparison rather than a restatement.
func TestEveryRecordedRequestIsOnARouteTheContractDeclares(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)

	checked := 0

	for _, scenario := range set.Order {
		for i, interaction := range set.Recordings[scenario].Interactions {
			recorded := interaction.Request
			path, _, _ := strings.Cut(recorded.Path, "?")

			route, ok := routeFor(recorded.Method, path)
			if !ok {
				t.Errorf("%s[%d]: %s %s is on no route in api.Routes", scenario, i, recorded.Method, path)

				continue
			}

			checked++

			if recorded.Headers.IdempotencyKeyPresent && !route.Idempotent {
				t.Errorf("%s[%d]: an Idempotency-Key on %s, which the contract does not give the parameter to",
					scenario, i, route.OpenAPIID)
			}
		}
	}

	if checked < 70 {
		t.Errorf("checked %d recorded requests against the route table; floor is 70", checked)
	}
}

func routeFor(method, path string) (api.Route, bool) {
	for _, route := range api.Routes {
		if !strings.EqualFold(route.Method, method) {
			continue
		}

		if templateMatches(route.Path, path) {
			return route, true
		}
	}

	return api.Route{}, false
}

func templateMatches(template, path string) bool {
	want := strings.Split(strings.Trim(template, "/"), "/")
	got := strings.Split(strings.Trim(path, "/"), "/")

	if len(want) != len(got) {
		return false
	}

	for i, segment := range want {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			if got[i] == "" {
				return false
			}

			continue
		}

		if segment != got[i] {
			return false
		}
	}

	return true
}

// ---------------------------------------------------------------------------
// AC30, AC31, AC32 — what the server answers
// ---------------------------------------------------------------------------

// The sweep: every committed interaction, driven as a Go client would send it,
// answered with exactly what was recorded and with no header that was not.
//
// It is the reachability floor for all three criteria — 72 scenarios and 79
// interactions at `54cccd1` — and it cannot pass by accident: `replay`
// re-serialises each request body from the decoded document, so every body it
// sends is in Go's key order rather than the recorder's.
func TestEveryRecordedInteractionIsServedAsRecorded(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)

	interactions := 0

	for _, scenario := range set.Order {
		recording := set.Recordings[scenario]

		t.Run(scenario, func(t *testing.T) {
			t.Parallel()

			server := fixture.New(t, scenario)

			for i, interaction := range recording.Interactions {
				resp := replay(t, server.URL(), interaction.Request, "")
				body := readBody(t, resp)

				if resp.StatusCode != interaction.Response.Status {
					t.Fatalf("interaction %d: answered %d (%s), recorded %d\n%s",
						i, resp.StatusCode, resp.Header.Get(fixture.UnrecordedHeader),
						interaction.Response.Status, body)
				}

				assertHeadersAreExactlyRecorded(t, i, resp, interaction.Response)
				assertBodyIsRecorded(t, i, body, interaction.Response.Body)
			}

			if left := server.Remaining(); len(left) != 0 {
				t.Errorf("interactions unplayed after the whole sequence: %v", left)
			}
		})

		interactions += len(recording.Interactions)
	}

	// Counted over the recordings the sweep scheduled a subtest for, which
	// is what makes it a floor: a set that had quietly shrunk would run
	// fewer subtests and every one of them would still pass.
	if interactions < 70 {
		t.Errorf("the sweep covers %d interactions; floor is 70", interactions)
	}
}

// AC32. The three headers named are the ones a client must never learn to
// expect from a fixture that invents them: a `Ferry-Command-Id` the API did
// not send is a command id the CLI would poll and a run record it would write.
func assertHeadersAreExactlyRecorded(t *testing.T, index int, resp *http.Response, recorded fixture.RecordedResponse) {
	t.Helper()

	// Content-Type, Content-Length and Date are the transport's, not
	// FERRY's; Go writes them for any response.
	transport := map[string]bool{"Content-Type": true, "Content-Length": true, "Date": true}

	// Header names are compared canonically. Go's `Header.Set` and its
	// client both canonicalise, so `WWW-Authenticate` arrives as
	// `Www-Authenticate`; the name's spelling is not a fact about FERRY's
	// answer and no client can observe it.
	canonical := make(map[string]string, len(recorded.Headers))
	for name, value := range recorded.Headers {
		canonical[http.CanonicalHeaderKey(name)] = value
	}

	for name, values := range resp.Header {
		if transport[name] {
			continue
		}

		want, ok := canonical[http.CanonicalHeaderKey(name)]
		if !ok {
			t.Errorf("interaction %d: the server sent %s: %v and the recording has no such header (AC32)", index, name, values)

			continue
		}

		if len(values) != 1 || values[0] != want {
			t.Errorf("interaction %d: %s is %v, recorded %q", index, name, values, want)
		}
	}

	for name, want := range recorded.Headers {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("interaction %d: %s is %q, recorded %q", index, name, got, want)
		}
	}
}

func assertBodyIsRecorded(t *testing.T, index int, got []byte, recorded json.RawMessage) {
	t.Helper()

	var want, have any

	if err := json.Unmarshal(recorded, &want); err != nil {
		t.Fatalf("interaction %d: the recorded body is not JSON: %v", index, err)
	}

	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatalf("interaction %d: the served body is not JSON: %v\n%s", index, err, got)
	}

	if !reflect.DeepEqual(want, have) {
		t.Errorf("interaction %d: the served body is not the recorded one", index)
	}
}

func TestUnrecordedRequestIs599WithTheHeader(t *testing.T) {
	t.Parallel()

	server := fixture.New(t, "me.api_key.200")

	resp := get(t, server.URL(), "/v1/nothing/here", apiKeyToken, "")
	defer resp.Body.Close()

	if resp.StatusCode != fixture.StatusUnrecorded {
		t.Fatalf("answered %d, want %d", resp.StatusCode, fixture.StatusUnrecorded)
	}

	if got, want := resp.Header.Get(fixture.UnrecordedHeader), "GET /v1/nothing/here"; got != want {
		t.Errorf("%s is %q, want %q", fixture.UnrecordedHeader, got, want)
	}

	if server.Unmatched() != 1 {
		t.Errorf("the server counted %d unmatched requests, want 1", server.Unmatched())
	}

	// The recording is still unplayed: an unmatched request must not consume
	// a sequence's next interaction.
	if left := server.Remaining()["me.api_key.200"]; left != 1 {
		t.Errorf("the unmatched request consumed an interaction (%d left)", left)
	}
}

// The status has to be the one the client exempts from retry, or every
// unrecorded-request test in every downstream package waits out a 60-second
// retry budget (CRITIQUE N5). Two authorities, compared.
func TestUnrecordedStatusIsTheOneTheClientNeverRetries(t *testing.T) {
	t.Parallel()

	if fixture.StatusUnrecorded != api.FixtureUnmatched {
		t.Fatalf("the fixture answers %d and api exempts %d from retry", fixture.StatusUnrecorded, api.FixtureUnmatched)
	}
}

// AC30. The money POST is matched on the body's structure; the perturbations
// are one key added, one key removed, one value changed. The baseline in the
// same test is the unperturbed body, so a server that answered 599 to
// everything would fail here rather than pass three rows out of four.
func TestMoneyPostIsMatchedOnTheStructureOfItsBody(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recorded := recordingFor(t, set, "execute.201.processing").Interactions[0].Request

	t.Run("the recorded body, re-serialised", func(t *testing.T) {
		t.Parallel()

		server := fixture.New(t, "execute.201.processing")

		resp := post(t, server.URL(), recorded.Path, mutate(t, recorded.Body, func(map[string]any) {}), "cli-test-key-1")
		defer resp.Body.Close()

		if resp.StatusCode != 201 {
			t.Fatalf("answered %d, want 201 (%s)", resp.StatusCode, resp.Header.Get(fixture.UnrecordedHeader))
		}
	})

	for _, row := range []struct {
		name string
		edit func(document map[string]any)
	}{
		{"an extra key", func(d map[string]any) { d["broadcast"] = true }},
		{"a missing key", func(d map[string]any) { delete(d, "plan_token") }},
		{"a different value", func(d map[string]any) { d["plan_token"] = "ferry_plan_SOMETHING_ELSE" }},
	} {
		t.Run(row.name+" is unrecorded", func(t *testing.T) {
			t.Parallel()

			server := fixture.New(t, "execute.201.processing")

			resp := post(t, server.URL(), recorded.Path, mutate(t, recorded.Body, row.edit), "cli-test-key-1")
			defer resp.Body.Close()

			if resp.StatusCode != fixture.StatusUnrecorded {
				t.Fatalf("answered %d, want %d: the fixture accepted a body the recording does not carry",
					resp.StatusCode, fixture.StatusUnrecorded)
			}

			if got, want := resp.Header.Get(fixture.UnrecordedHeader), "POST /v1/transfers"; got != want {
				t.Errorf("%s is %q, want %q", fixture.UnrecordedHeader, got, want)
			}
		})
	}
}

func TestKeyOrderAndWhitespaceAreNotPartOfTheMatch(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recorded := recordingFor(t, set, "simulate.201").Interactions[0].Request

	var document map[string]any
	if err := json.Unmarshal(recorded.Body, &document); err != nil {
		t.Fatalf("the recorded body is not an object: %v", err)
	}

	indented, err := json.MarshalIndent(document, "", "\t")
	if err != nil {
		t.Fatalf("re-serialising: %v", err)
	}

	server := fixture.New(t, "simulate.201")

	resp := post(t, server.URL(), recorded.Path, indented, "cli-test-key-1")
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		t.Fatalf("answered %d, want 201: whitespace and key order are not semantic (§5.10)", resp.StatusCode)
	}
}

func TestIdempotencyKeyPresenceIsPartOfTheMatch(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)

	t.Run("a money POST sent without the key", func(t *testing.T) {
		t.Parallel()

		recorded := recordingFor(t, set, "execute.201.processing").Interactions[0].Request
		server := fixture.New(t, "execute.201.processing")

		resp := post(t, server.URL(), recorded.Path, mutate(t, recorded.Body, func(map[string]any) {}), "")
		defer resp.Body.Close()

		if resp.StatusCode != fixture.StatusUnrecorded {
			t.Fatalf("answered %d, want %d: the recording was made with an Idempotency-Key",
				resp.StatusCode, fixture.StatusUnrecorded)
		}
	})

	t.Run("a read sent with one", func(t *testing.T) {
		t.Parallel()

		server := fixture.New(t, "me.api_key.200")

		resp := get(t, server.URL(), "/v1/me", apiKeyToken, "cli-test-key-1")
		defer resp.Body.Close()

		if resp.StatusCode != fixture.StatusUnrecorded {
			t.Fatalf("answered %d, want %d: the recording was made without an Idempotency-Key",
				resp.StatusCode, fixture.StatusUnrecorded)
		}
	})
}

// A331. The recordings carry the credential class and FERRY decides on it
// before anything else (§1.2.1); a fixture that ignored it would answer a PAT
// with the 201 the API reserves for an API key.
func TestCredentialClassIsPartOfTheMatch(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recorded := recordingFor(t, set, "execute.201.processing").Interactions[0].Request
	body := mutate(t, recorded.Body, func(map[string]any) {})

	for _, row := range []struct {
		name  string
		token string
	}{
		{"a personal access token", patToken},
		{"a token of no recognised shape", "not-a-ferry-token"},
		{"no credential at all", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			server := fixture.New(t, "execute.201.processing")

			req, err := http.NewRequest(http.MethodPost, server.URL()+recorded.Path, strings.NewReader(string(body)))
			if err != nil {
				t.Fatalf("building the request: %v", err)
			}

			if row.token != "" {
				req.Header.Set("Authorization", "Bearer "+row.token)
			}

			req.Header.Set("Idempotency-Key", "cli-test-key-1")
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("sending: %v", err)
			}

			defer resp.Body.Close()

			if resp.StatusCode != fixture.StatusUnrecorded {
				t.Fatalf("answered %d, want %d: the recording was made with an API key",
					resp.StatusCode, fixture.StatusUnrecorded)
			}
		})
	}
}

// The path is matched as the escaped request target, so the corridor id that
// AC40 is about — `walletCrypto->bankUs` — reaches the recording that was
// made for it and a different corridor does not.
//
// The second half is a measurement, not an assertion of a difference: Go's
// URL layer escapes `>` when it writes the request line, so a client that
// *skipped* `url.PathEscape` puts the identical bytes on the wire and the
// fixture cannot tell the two spellings apart. M41's detector therefore
// cannot be a 599 here; it has to be an assertion on what `api.Expand`
// returns. Reported to U4 as A332.
func TestThePathIsMatchedAsTheEscapedRequestTarget(t *testing.T) {
	t.Parallel()

	server := fixture.New(t, "corridors.get.walletCrypto_bankUs.200", "corridors.get.walletCrypto_bankUs.200")

	encoded := get(t, server.URL(), "/v1/corridors/walletCrypto-%3EbankUs", apiKeyToken, "")
	defer encoded.Body.Close()

	if encoded.StatusCode != 200 {
		t.Fatalf("the encoded target answered %d, want 200", encoded.StatusCode)
	}

	other := get(t, server.URL(), "/v1/corridors/walletCrypto-%3Ecash", apiKeyToken, "")
	defer other.Body.Close()

	if other.StatusCode != fixture.StatusUnrecorded {
		t.Fatalf("a different corridor answered %d, want %d", other.StatusCode, fixture.StatusUnrecorded)
	}

	raw := get(t, server.URL(), "/v1/corridors/walletCrypto->bankUs", apiKeyToken, "")
	defer raw.Body.Close()

	if raw.StatusCode != 200 {
		t.Fatalf("the unescaped spelling answered %d; net/url escapes it on the wire, so it is the same request (%d expected)",
			raw.StatusCode, 200)
	}

	log := server.Requests()
	if len(log) != 3 {
		t.Fatalf("the log has %d requests, want 3", len(log))
	}

	if log[2].Path != "/v1/corridors/walletCrypto-%3EbankUs" {
		t.Errorf("the unescaped spelling arrived as %q; the measurement above no longer holds", log[2].Path)
	}
}

// The client builds its query with url.Values.Encode, which sorts by key; the
// recorder wrote `?limit=2&cursor=…`. Neither should have to know the other's
// ordering, and a mismatched *value* must still be unrecorded.
func TestQueryIsComparedByValueNotByOrder(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recorded := recordingFor(t, set, "keys.list.page2.200").Interactions[0].Request

	path, query, found := strings.Cut(recorded.Path, "?")
	if !found {
		t.Fatalf("keys.list.page2.200 has no query in %q", recorded.Path)
	}

	fields := strings.Split(query, "&")
	if len(fields) != 2 {
		t.Fatalf("expected two query fields in %q", query)
	}

	// A server each. Sharing one would let the second request be answered
	// 599 because the first had already consumed the only interaction,
	// which is the right status for the wrong reason — the mutation
	// "compare no query at all" survived against exactly that shape.
	reordered := fixture.New(t, "keys.list.page2.200")

	swapped := get(t, reordered.URL(), path+"?"+fields[1]+"&"+fields[0], patToken, "")
	defer swapped.Body.Close()

	if swapped.StatusCode != 200 {
		t.Fatalf("the reordered query answered %d, want 200", swapped.StatusCode)
	}

	other := fixture.New(t, "keys.list.page2.200")

	changed := get(t, other.URL(), path+"?"+strings.Replace(query, "limit=2", "limit=3", 1), patToken, "")
	defer changed.Body.Close()

	if changed.StatusCode != fixture.StatusUnrecorded {
		t.Fatalf("a different query value answered %d, want %d", changed.StatusCode, fixture.StatusUnrecorded)
	}
}

// AC31. The three answers of `execute.202.then_completed` are a 202 and two
// polls, and the second poll is the one that says `completed`. A server that
// always served the first interaction would answer `upstream_unknown` forever
// — which is M33, and is why the states are read out of the bodies here.
func TestASequenceIsReplayedInOrder(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recording := recordingFor(t, set, "execute.202.then_completed")

	if len(recording.Interactions) != 3 {
		t.Fatalf("expected a three-interaction sequence, got %d", len(recording.Interactions))
	}

	server := fixture.New(t, "execute.202.then_completed")

	var states []string

	for i, interaction := range recording.Interactions {
		resp := replay(t, server.URL(), interaction.Request, "")

		if resp.StatusCode != interaction.Response.Status {
			t.Fatalf("interaction %d answered %d, recorded %d", i, resp.StatusCode, interaction.Response.Status)
		}

		body := decodeBody(t, resp)

		state, _ := body["state"].(string)
		states = append(states, state)
	}

	if want := []string{"upstream_unknown", "upstream_unknown", "completed"}; !reflect.DeepEqual(states, want) {
		t.Fatalf("states were %v, want %v", states, want)
	}

	// A fourth poll is not recorded. The sequence is over, and the fixture
	// says so rather than serving the last answer again.
	fourth := replay(t, server.URL(), recording.Interactions[2].Request, "")
	defer fourth.Body.Close()

	if fourth.StatusCode != fixture.StatusUnrecorded {
		t.Fatalf("a fourth poll answered %d, want %d", fourth.StatusCode, fixture.StatusUnrecorded)
	}
}

// An out-of-order request inside a sequence is unrecorded, even though the
// same request appears later in the same recording. A fixture that searched
// the whole sequence would answer `completed` to the first poll of a command
// that is still `upstream_unknown`.
func TestASequenceDoesNotAnswerOutOfOrder(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recording := recordingFor(t, set, "execute.202.then_completed")

	server := fixture.New(t, "execute.202.then_completed")

	resp := replay(t, server.URL(), recording.Interactions[1].Request, "")
	defer resp.Body.Close()

	if resp.StatusCode != fixture.StatusUnrecorded {
		t.Fatalf("the poll answered %d before the execute was sent, want %d", resp.StatusCode, fixture.StatusUnrecorded)
	}
}

// AC31's log: count, spacing, keys, and byte identity between two requests.
func TestTheRequestLogRecordsBytesKeysAndSpacing(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recorded := recordingFor(t, set, "execute.201.processing").Interactions[0].Request
	body := mutate(t, recorded.Body, func(map[string]any) {})

	// The same scenario twice: two tracks, so the second send is answered
	// the way a resume's would be.
	server := fixture.New(t, "execute.201.processing", "execute.201.processing")

	first := post(t, server.URL(), recorded.Path, body, "cli-test-key-1")
	first.Body.Close()

	time.Sleep(2 * time.Millisecond)

	second := post(t, server.URL(), recorded.Path, body, "cli-test-key-1")
	second.Body.Close()

	log := server.Requests()

	if len(log) != 2 {
		t.Fatalf("the log has %d requests, want 2", len(log))
	}

	if log[0].SHA256 != log[1].SHA256 || string(log[0].Body) != string(log[1].Body) {
		t.Errorf("the two requests are not byte-identical:\n%s\n%s", log[0].Body, log[1].Body)
	}

	// The digest is the digest of those bytes, computed here rather than
	// taken on trust: a truncated or constant one is equal across two
	// identical requests too, and would pass the comparison above.
	sum := sha256.Sum256(body)
	if want := hex.EncodeToString(sum[:]); log[0].SHA256 != want {
		t.Errorf("the logged digest is %q, want %q", log[0].SHA256, want)
	}

	if string(log[0].Body) != string(body) {
		t.Errorf("the log kept %q, the client sent %q", log[0].Body, body)
	}

	if log[0].IdempotencyKey() != "cli-test-key-1" || log[1].IdempotencyKey() != "cli-test-key-1" {
		t.Errorf("keys were %q and %q", log[0].IdempotencyKey(), log[1].IdempotencyKey())
	}

	if !log[1].At.After(log[0].At) {
		t.Errorf("the log cannot say how the two requests were spaced: %v then %v", log[0].At, log[1].At)
	}

	for i, entry := range log {
		if !entry.Matched() || entry.Scenario != "execute.201.processing" {
			t.Errorf("request %d was answered by %q", i, entry.Scenario)
		}

		if entry.Status != 201 {
			t.Errorf("request %d was answered %d", i, entry.Status)
		}
	}

	if got := log[0].Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("the log did not keep the request headers (Content-Type was %q)", got)
	}

	// A request that matched nothing is logged too, with its own bytes: a
	// test asking "what did the CLI send" must not have to hope the fixture
	// recognised it.
	different := mutate(t, recorded.Body, func(d map[string]any) { d["plan_token"] = "ferry_plan_OTHER" })

	unmatched := post(t, server.URL(), recorded.Path, different, "cli-test-key-1")
	unmatched.Body.Close()

	log = server.Requests()

	if len(log) != 3 {
		t.Fatalf("the log has %d requests, want 3", len(log))
	}

	if log[2].Matched() {
		t.Errorf("the third request was answered by %q", log[2].Scenario)
	}

	if log[2].SHA256 == log[0].SHA256 {
		t.Errorf("two different bodies have the same logged digest %q", log[2].SHA256)
	}

	if string(log[2].Body) != string(different) {
		t.Errorf("the unmatched request's bytes were not logged")
	}
}

// AC32, stated as a refusal rather than as an equality: the scenario below
// records no `Ferry-Command-Id` — `auth.wrong_class.transfers_with_pat.403`
// declares it absent — and sends an `Idempotency-Key` the app never echoed.
func TestTheServerInventsNoCommandIdRetryAfterOrReplayHeader(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recording := recordingFor(t, set, "auth.wrong_class.transfers_with_pat.403")
	recorded := recording.Interactions[0]

	for _, name := range []string{"Ferry-Command-Id", "Retry-After", "Idempotency-Replayed", "Idempotency-Key"} {
		if _, ok := recorded.Response.Headers[name]; ok {
			t.Fatalf("this control needs a recording without %s; the recording has one", name)
		}
	}

	server := fixture.New(t, "auth.wrong_class.transfers_with_pat.403")

	resp := replay(t, server.URL(), recorded.Request, "cli-test-key-1")
	defer resp.Body.Close()

	if resp.StatusCode != 403 {
		t.Fatalf("answered %d, want 403", resp.StatusCode)
	}

	for _, name := range []string{"Ferry-Command-Id", "Retry-After", "Idempotency-Replayed", "Idempotency-Key"} {
		if got := resp.Header.Get(name); got != "" {
			t.Errorf("the server sent %s: %q and the recording has no such header (AC32)", name, got)
		}
	}
}

func TestRecordedHeadersAreServedExactly(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recorded := recordingFor(t, set, "execute.202.then_completed").Interactions[0]

	for _, name := range []string{"Ferry-Command-Id", "Retry-After", "Idempotency-Key"} {
		if _, ok := recorded.Response.Headers[name]; !ok {
			t.Fatalf("this control needs a recording carrying %s", name)
		}
	}

	server := fixture.New(t, "execute.202.then_completed")

	resp := replay(t, server.URL(), recorded.Request, "")
	defer resp.Body.Close()

	for name, want := range recorded.Response.Headers {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s is %q, recorded %q", name, got, want)
		}
	}
}

func TestAServerWithNoRecordingsAnswersNothing(t *testing.T) {
	t.Parallel()

	server := fixture.New(t)

	resp := get(t, server.URL(), "/v1/me", apiKeyToken, "")
	defer resp.Body.Close()

	if resp.StatusCode != fixture.StatusUnrecorded {
		t.Fatalf("answered %d, want %d", resp.StatusCode, fixture.StatusUnrecorded)
	}

	if len(server.Requests()) != 1 {
		t.Errorf("the log has %d requests, want 1", len(server.Requests()))
	}
}

// ---------------------------------------------------------------------------
// AC91 — holding a request open
// ---------------------------------------------------------------------------

// The mechanism U5 needs to deliver a signal to a client whose money request
// is on the wire (C17).
//
// The control is an ordering, and it is written so that the mutation M98
// names — "Hold releases immediately" — is red for the right reason: after
// `arrived` fires, the answer must *not* arrive, and the test waits long
// enough to say so. The wait can only go red if the server answered while
// held, which is the defect; it cannot go red on a slow machine, because
// nothing releases it.
func TestHoldKeepsTheRequestOnTheWireUntilReleased(t *testing.T) {
	t.Parallel()

	set := recordingsOf(t)
	recorded := recordingFor(t, set, "execute.201.processing").Interactions[0].Request
	body := mutate(t, recorded.Body, func(map[string]any) {})

	server := fixture.New(t, "execute.201.processing")
	arrived, release := server.Hold("execute.201.processing")

	answered := make(chan int, 1)

	go func() {
		resp := post(t, server.URL(), recorded.Path, body, "cli-test-key-1")
		defer resp.Body.Close()

		answered <- resp.StatusCode
	}()

	select {
	case <-arrived:
	case status := <-answered:
		t.Fatalf("the request was answered (%d) before the hold saw it", status)
	case <-time.After(10 * time.Second):
		t.Fatalf("the held request never reached the server")
	}

	// Witnessed, not inferred: the server has the request and the client
	// does not have an answer.
	if log := server.Requests(); len(log) != 1 {
		t.Fatalf("the server logged %d requests while holding one", len(log))
	}

	const window = 250 * time.Millisecond

	select {
	case status := <-answered:
		t.Fatalf("the held request was answered %d without being released (AC91)", status)
	case <-time.After(window):
	}

	release()

	select {
	case status := <-answered:
		if status != 201 {
			t.Fatalf("the released request answered %d, want 201", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the request was never answered after release")
	}

	// Releasing twice is what a deferred release does after an explicit one.
	release()
}

func TestHoldAffectsOnlyItsOwnScenario(t *testing.T) {
	t.Parallel()

	server := fixture.New(t, "me.api_key.200", "execute.201.processing")
	_, release := server.Hold("execute.201.processing")

	defer release()

	resp := get(t, server.URL(), "/v1/me", apiKeyToken, "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("the unheld scenario answered %d, want 200", resp.StatusCode)
	}
}

func manifestExpect(t *testing.T, set *fixture.Set, scenario string) fixture.Expectation {
	t.Helper()

	for _, entry := range set.Manifest.Scenarios {
		if entry.Scenario == scenario {
			if len(entry.Expect) == 0 {
				t.Fatalf("%s declares no expectation", scenario)
			}

			return entry.Expect[0]
		}
	}

	t.Fatalf("the manifest does not name %s", scenario)

	return fixture.Expectation{}
}

func sorted(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	sort.Strings(out)

	return out
}
