package corridors_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/kurenn/ferry/cli/internal/api"
	"github.com/kurenn/ferry/cli/internal/render"
)

// ---------------------------------------------------------------------------
// AC40, first half — the `>` in a corridor id is percent-encoded in the path.
// ---------------------------------------------------------------------------

// The encoding is asserted on what `api.Expand` returns, and this comment is
// the reason why (A332).
//
// `net/url` escapes a raw `>` when it writes the request line, so
// `walletCrypto->bankUs` and `walletCrypto-%3EbankUs` put *identical bytes* on
// the wire. U3 measured this; the measurement below repeats it. A test that
// asserted the fixture received `/v1/corridors/walletCrypto-%3EbankUs` would
// therefore be green with the encoding removed — the vacuous control this
// project has shipped before — because `net/http` would put it back.
//
// The layer where the defect is observable is `api.Expand`'s return value,
// which is the last point at which the CLI's own decision is visible. So that
// is where §6.2's M41 ("skip encoding `->`") is caught.
//
// `api.Expand` is U2's and is not edited here; what U4 owns is the decision to
// hand the id to it as a path parameter rather than splicing it into the path,
// and the assertion that the result is encoded.
func TestTheCorridorIDIsPercentEncodedInThePath(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"walletCrypto->bankUs", "/v1/corridors/walletCrypto-%3EbankUs"},
		{"walletCrypto->cash", "/v1/corridors/walletCrypto-%3Ecash"},
		{"card->cash", "/v1/corridors/card-%3Ecash"},
	}

	for _, tc := range cases {
		got, err := api.Expand("/v1/corridors/{id}", map[string]string{"id": tc.id})
		if err != nil {
			t.Fatalf("Expand(%q): %v", tc.id, err)
		}

		if got != tc.want {
			t.Errorf("Expand(%q) is %q, want %q", tc.id, got, tc.want)
		}

		if strings.Contains(got, ">") {
			t.Errorf("Expand(%q) left a raw `>` in the path: %q", tc.id, got)
		}
	}
}

// And the measurement that justifies the layer, taken rather than asserted
// from memory.
//
// If this ever stops holding — if `net/url` stops normalising the raw form —
// then an HTTP-level assertion would become meaningful and the test above
// could move. Until then it would be a control that cannot fail.
func TestTheRawAndEncodedSpellingsAreTheSameBytesOnTheWire(t *testing.T) {
	raw, err := url.Parse("http://example.test/v1/corridors/walletCrypto->bankUs")
	if err != nil {
		t.Fatalf("parse raw: %v", err)
	}

	encoded, err := url.Parse("http://example.test/v1/corridors/walletCrypto-%3EbankUs")
	if err != nil {
		t.Fatalf("parse encoded: %v", err)
	}

	if raw.RequestURI() != encoded.RequestURI() {
		t.Fatalf("the two spellings are distinguishable on the wire (%q vs %q); AC40 can and "+
			"should then be asserted over HTTP rather than at api.Expand",
			raw.RequestURI(), encoded.RequestURI())
	}

	if got := raw.RequestURI(); got != "/v1/corridors/walletCrypto-%3EbankUs" {
		t.Errorf("the request target is %q", got)
	}
}

// The command end-to-end: the id reaches the route as a path parameter and the
// recorded escaped target is what the fixture matched.
func TestGetFetchesTheCorridorByItsID(t *testing.T) {
	server, home := loggedIn(t, "corridors.get.walletCrypto_bankUs.200")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"get", "walletCrypto->bankUs", "--api", server.URL()},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	req := server.Requests()[0]

	if req.Path != "/v1/corridors/walletCrypto-%3EbankUs" {
		t.Errorf("the fixture saw %q", req.Path)
	}

	if !strings.Contains(stdout, "walletCrypto->bankUs") {
		t.Errorf("the corridor's id is not shown:\n%s", stdout)
	}
}

func TestAnUnknownCorridorIsFERRYsRefusal(t *testing.T) {
	server, home := loggedIn(t, "corridors.get.unknown.404")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"get", "card->cash", "--api", server.URL()},
	})

	if exit == 0 {
		t.Fatalf("exit 0 on a 404\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	if !strings.Contains(stderr, "NOT_FOUND") {
		t.Errorf("the refusal does not carry FERRY's code: %q", stderr)
	}

	if n := requestCount(server); n != 1 {
		t.Errorf("the fixture saw %d requests, want 1", n)
	}
}

func TestGetWithoutAnIDIsAUsageError(t *testing.T) {
	server, home := loggedIn(t, "corridors.get.walletCrypto_bankUs.200")

	_, stderr, exit := run(t, invocation{home: home, args: []string{"get", "--api", server.URL()}})

	if exit != 2 {
		t.Errorf("exit %d, want 2", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests", n)
	}

	if !strings.Contains(stderr, "exactly 1 argument") {
		t.Errorf("the refusal reads %q", stderr)
	}
}

// ---------------------------------------------------------------------------
// AC40, second half — `null` and `[]` are different answers and render
// differently.
// ---------------------------------------------------------------------------

// The two spellings are both present in one recorded corridor, which is what
// makes this testable at all: `walletCrypto->cash` has `allowed_assets: null`
// on its source and `allowed_networks: []` on its destination.
//
// The distinction is not cosmetic. `null` means the contract enumerates
// nothing — any asset is allowed as far as this corridor is concerned — and
// `[]` means it enumerates the empty set: none is. A renderer that printed the
// same words for both would tell a caller they may send nothing when they may
// send anything, or the reverse.
//
// §6.2 names no mutation for this; the one this unit ran (U4-3 in the PR body)
// collapses the two spellings, and both halves of this test redden.
func TestNullAndEmptyRenderAsDifferentAnswers(t *testing.T) {
	server, home := loggedIn(t, "corridors.get.walletCrypto_cash.200")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"get", "walletCrypto->cash", "--api", server.URL()},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	if render.NullList == render.EmptyList {
		t.Fatal("render.NullList and render.EmptyList are the same string, so every assertion " +
			"below is satisfied by a renderer that cannot tell them apart")
	}

	if !strings.Contains(stdout, render.NullList) {
		t.Errorf("the source's `allowed_assets: null` does not render as %q:\n%s", render.NullList, stdout)
	}

	if !strings.Contains(stdout, render.EmptyList) {
		t.Errorf("the destination's `allowed_networks: []` does not render as %q:\n%s",
			render.EmptyList, stdout)
	}
}

// The renderer's own table, stated directly, so the three cases are pinned
// even if no recording happens to carry one of them.
func TestTheThreeListSpellingsAreThreeDifferentRenders(t *testing.T) {
	absent := render.List(api.StringList{})
	empty := render.List(api.StringList{Present: true, Values: []string{}})
	populated := render.List(api.StringList{Present: true, Values: []string{"usd", "eur"}})

	seen := map[string]string{absent: "null", empty: "[]", populated: `["usd","eur"]`}
	if len(seen) != 3 {
		t.Fatalf("the three spellings render as %d distinct strings: null=%q, []=%q, values=%q",
			len(seen), absent, empty, populated)
	}

	if populated != "usd, eur" {
		t.Errorf("a populated list renders as %q", populated)
	}
}

// And the JSON document keeps the distinction too: `--output json` echoes the
// API body verbatim, so a caller parsing it sees `null` and `[]` exactly as
// FERRY sent them.
func TestTheJSONDocumentPreservesNullAndEmpty(t *testing.T) {
	server, home := loggedIn(t, "corridors.get.walletCrypto_cash.200")

	stdout, _, exit := run(t, invocation{
		home: home,
		args: []string{"get", "walletCrypto->cash", "--api", server.URL(), "--output", "json"},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0", exit)
	}

	var corridor struct {
		Source struct {
			AllowedAssets   json.RawMessage `json:"allowed_assets"`
			AllowedNetworks json.RawMessage `json:"allowed_networks"`
		} `json:"source"`
		Destination struct {
			AllowedAssets   json.RawMessage `json:"allowed_assets"`
			AllowedNetworks json.RawMessage `json:"allowed_networks"`
		} `json:"destination"`
	}

	if err := json.Unmarshal(response(t, stdout), &corridor); err != nil {
		t.Fatalf("parse response: %v", err)
	}

	if got := string(corridor.Source.AllowedAssets); got != "null" {
		t.Errorf("source.allowed_assets is %q, want null", got)
	}

	if got := string(corridor.Destination.AllowedNetworks); got != "[]" {
		t.Errorf("destination.allowed_networks is %q, want []", got)
	}
}

// ---------------------------------------------------------------------------
// `corridors list`.
// ---------------------------------------------------------------------------

func TestListShowsEveryCorridorTheAnswerCarried(t *testing.T) {
	server, home := loggedIn(t, "corridors.list.200")

	stdout, stderr, exit := run(t, invocation{home: home, args: []string{"list", "--api", server.URL()}})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	if n := requestCount(server); n != 1 {
		t.Fatalf("the fixture saw %d requests, want 1", n)
	}

	if req := server.Requests()[0]; req.Path != "/v1/corridors" || len(req.Query) != 0 {
		t.Errorf("list sent %s?%s; `GET /v1/corridors` takes no parameters", req.Path, req.RawQuery)
	}

	// Every id in the body appears in the render, both directions.
	var doc struct {
		Response struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		} `json:"response"`
	}

	// A second server, because the recording has one interaction and a
	// second request against the first would be answered 599.
	jsonServer, jsonHome := loggedIn(t, "corridors.list.200")

	jsonOut, _, exit := run(t, invocation{
		home: jsonHome,
		args: []string{"list", "--api", jsonServer.URL(), "--output", "json"},
	})
	if exit != 0 {
		t.Fatalf("the json run exits %d", exit)
	}

	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
		t.Fatalf("parse document: %v\n%s", err, jsonOut)
	}

	if len(doc.Response.Data) == 0 {
		t.Fatal("the answer carried no corridors, so the comparison below is two empty sets")
	}

	for _, c := range doc.Response.Data {
		if !strings.Contains(stdout, c.ID) {
			t.Errorf("the text render omits corridor %q", c.ID)
		}
	}

	if got := strings.Count(stdout, "->"); got != len(doc.Response.Data) {
		t.Errorf("the text render shows %d corridor ids and the answer carried %d",
			got, len(doc.Response.Data))
	}
}
