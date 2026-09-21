package transfers_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// AC56 and AC78: how the request body is built, and what happens when two
// ways of building it are given at once.

// The flags produce exactly the recorded body.
//
// The fixture compares bodies structurally and answers 599 for anything else
// (AC30), so every test in this package that reaches a 201 already depends
// on this. What this test adds is the *field-by-field* comparison, so that a
// failure says which field is wrong rather than "the fixture refused".
func TestTheFlagsProduceTheRecordedBody(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	if _, _, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
	}); exit != 0 {
		t.Fatalf("the simulate did not settle")
	}

	sent := bodyOfRequestTo(t, server, simulatePath, 0)

	var got map[string]any
	if err := json.Unmarshal([]byte(sent), &got); err != nil {
		t.Fatalf("the CLI sent something that is not JSON: %v\n%s", err, sent)
	}

	// The recorded request body, read from the recording rather than
	// restated here. Restating it would make this a test of my own
	// transcription (C13: one authority).
	want := recordedRequestBody(t, "simulate.201", "POST", simulatePath)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the body does not match the recording.\n sent: %s\nwant: %v", sent, want)
	}

	// Both directions on the key sets, with the difference named, because
	// `DeepEqual` on maps says only "not equal".
	for _, key := range keysOf(want) {
		if _, ok := got[key]; !ok {
			t.Errorf("the body is missing %q, which the recording has", key)
		}
	}

	for _, key := range keysOf(got) {
		if _, ok := want[key]; !ok {
			t.Errorf("the body has %q, which the recording does not", key)
		}
	}
}

// AC56: `--body -` reads the whole request from stdin.
func TestTheBodyCanComeFromStdin(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	body := recordedRequestBodyRaw(t, "simulate.201", "POST", simulatePath)

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		stdin: body,
		args:  []string{"transfers", "create", "--body", "-"},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, simulatePath); got != 1 {
		t.Fatalf("simulate requests = %d, want 1", got)
	}
}

// AC56: `--body FILE` reads it from a file.
func TestTheBodyCanComeFromAFile(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	path := filepath.Join(t.TempDir(), "body.json")

	body := recordedRequestBodyRaw(t, "simulate.201", "POST", simulatePath)

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "create", "--body", path},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, simulatePath); got != 1 {
		t.Errorf("simulate requests = %d, want 1", got)
	}
}

// AC78: `--body` together with a field flag is exit 2, and nothing is sent.
//
// The two are different authorities over the same bytes. Merging them means
// deciding which wins for every field, and getting that wrong silently
// changes an amount. Refusing is the only answer that cannot move the wrong
// number of dollars.
// AC56's second clause, stated exactly: **`--body` bytes are forwarded
// unchanged, and non-canonical whitespace survives to the wire.**
//
// It matters because of what the alternative would mean. A CLI that
// re-encoded the caller's bytes would be deciding the request's canonical
// form, and `Idempotency-Key` is paired with a digest of the body: two
// invocations that sent the same JSON with different spacing would produce
// two digests, and the second would be refused as a key reused with a
// different request — or, worse, a caller's careful bytes would be replaced
// by this CLI's idea of them and the digest on disk would not be the digest
// of what they wrote.
//
// A346's rule about layers applies: the assertion is on the bytes the
// fixture received, byte for byte, because a comparison of parsed JSON is
// exactly the comparison that cannot see the difference.
func TestNonCanonicalWhitespaceInABodySurvivesToTheWire(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	// The recording's own body, reformatted with spacing no encoder
	// produces: indented two spaces, a space before each colon, a
	// trailing newline. `--body` takes a TransferRequest, which is what
	// simulate sends.
	recorded := recordedRequestBodyRaw(t, "simulate.201", "POST", simulatePath)

	var parsed any
	if err := json.Unmarshal([]byte(recorded), &parsed); err != nil {
		t.Fatalf("the recording's body is not valid JSON: %v", err)
	}

	indented, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		t.Fatalf("indent: %v", err)
	}

	spaced := string(indented)

	// Fed with a trailing newline, which is what a heredoc, an `echo` or
	// a text editor leaves. It is trimmed — see the assertion at the end
	// — and everything inside the document is not.
	stdout, stderr, exit := run(t, invocation{
		home:  home,
		args:  []string{"transfers", "create", "--body", "-"},
		stdin: spaced + "\n",
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0. The fixture compares bodies structurally, so spacing "+
			"is not why this would fail.\n%s\n%s", exit, stdout, stderr)
	}

	got := bodyOfRequestTo(t, server, simulatePath, 0)

	if got != spaced {
		t.Errorf("the body on the wire is not the body the caller gave.\n got %q\nwant %q\n"+
			"The digest pinned in the run ledger is a digest of these bytes and a resend "+
			"sends them again (AC11, AC45); re-encoding here makes the record a record "+
			"of something the caller never wrote.", got, spaced)
	}

	// The floor. If `spaced` were already canonical the comparison above
	// would pass against a CLI that re-encoded every body, which is
	// exactly mutation M55.
	compact, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if string(compact) == spaced {
		t.Fatal("the test's body is already canonical, so the assertion above would be " +
			"satisfied by a CLI that re-encodes")
	}

	// The one transformation there is, stated rather than discovered:
	// surrounding whitespace is trimmed, so `ferry … --body - <<'EOF'`
	// sends the document and not the document plus a newline. Everything
	// between the first `{` and the last `}` is the caller's.
	if strings.HasSuffix(got, "\n") {
		t.Errorf("the trailing newline reached the wire; it is trimmed so that a heredoc "+
			"and a file with no final newline produce the same digest\n%q", got)
	}
}

func TestBodyTogetherWithAFieldFlagIsAUsageError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{name: "an amount", extra: []string{"--amount", "100.00"}},
		{name: "a customer", extra: []string{"--customer", "cst_PLACEHOLDER_1"}},
		{name: "one metadata pair", extra: []string{"--metadata", "k=v"}},
		{name: "a source asset", extra: []string{"--from-asset", "usdc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, home := loggedIn(t, "simulate.201")

			args := append([]string{"transfers", "create", "--body", "-"}, tc.extra...)

			stdout, stderr, exit := run(t, invocation{
				home:  home,
				stdin: `{"customer_id":"cst_PLACEHOLDER_1"}`,
				args:  args,
			})

			if exit != 2 {
				t.Errorf("exit = %d, want 2\n%s\n%s", exit, stdout, stderr)
			}

			if got := len(server.Requests()); got != 0 {
				t.Errorf("the fixture saw %d request(s), want 0", got)
			}

			// The message has to name the flag that conflicted, or the
			// caller has to bisect their own command line.
			flag := strings.TrimPrefix(tc.extra[0], "--")

			if !strings.Contains(stdout+stderr, flag) {
				t.Errorf("the refusal does not name %q\n%s\n%s", flag, stdout, stderr)
			}
		})
	}
}

// The control for AC78: flags that are not body fields are fine alongside
// `--body`.
//
// `--output`, `--profile`, `--yes` and `--idempotency-key` say nothing about
// the request body, so refusing them would make `--body` unusable from a
// script — which is the only place `--body` is ever used.
func TestBodyIsCompatibleWithFlagsThatAreNotBodyFields(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	body := recordedRequestBodyRaw(t, "simulate.201", "POST", simulatePath)

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		stdin: body,
		args: []string{"transfers", "create", "--body", "-",
			"--output", "json", "--profile", "default", "--idempotency-key", "k1"},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, simulatePath); got != 1 {
		t.Errorf("simulate requests = %d, want 1", got)
	}

	if got := keysSentTo(server, simulatePath); got[0] != "k1" {
		t.Errorf("the key sent was %q, want the caller's k1", got[0])
	}
}

// A `--body` that is not JSON is refused locally.
//
// Sending it would spend a round trip and get a `400` whose message is about
// FERRY's parser rather than about the caller's file.
func TestAnUnparseableBodyIsRefusedWithoutSending(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "not JSON", body: "{oh dear"},
		{name: "empty", body: ""},
		{name: "a JSON array", body: `[1,2,3]`},
		{name: "a JSON string", body: `"a body"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, home := loggedIn(t, "simulate.201")

			stdout, stderr, exit := run(t, invocation{
				home:  home,
				stdin: tc.body,
				args:  []string{"transfers", "create", "--body", "-"},
			})

			if exit != 2 {
				t.Errorf("exit = %d, want 2\n%s\n%s", exit, stdout, stderr)
			}

			if got := len(server.Requests()); got != 0 {
				t.Errorf("the fixture saw %d request(s), want 0", got)
			}
		})
	}
}

// A `--body FILE` naming a file that is not there is exit 2, not exit 1.
//
// It is the caller's command line that is wrong, and exit 2 is what a
// wrapper reads as "I typed this wrong". Exit 1 would say "the CLI has a
// local problem", which invites a retry.
func TestAMissingBodyFileIsAUsageError(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	path := filepath.Join(t.TempDir(), "absent.json")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "create", "--body", path},
	})

	if exit != 2 {
		t.Errorf("exit = %d, want 2\n%s\n%s", exit, stdout, stderr)
	}

	if got := len(server.Requests()); got != 0 {
		t.Errorf("the fixture saw %d request(s), want 0", got)
	}

	if !strings.Contains(stdout+stderr, path) {
		t.Errorf("the refusal does not name the file\n%s\n%s", stdout, stderr)
	}
}

// Metadata is `k=v`, repeatable, and a pair without an `=` is refused.
//
// Silently dropping it would send a transfer whose metadata is missing the
// key the caller is going to reconcile against.
func TestMetadataPairsAreValidated(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pair     string
		wantExit int
	}{
		{name: "no equals", pair: "orderid", wantExit: 2},
		{name: "an empty key", pair: "=v", wantExit: 2},
		{name: "a value containing equals", pair: "k=a=b", wantExit: 0},
		{name: "an empty value", pair: "k=", wantExit: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, home := loggedIn(t, "simulate.201")

			args := append([]string{"transfers", "create"}, canonicalSimulateArgs()...)
			args = append(args, "--metadata", tc.pair)

			stdout, stderr, exit := run(t, invocation{home: home, args: args})

			if tc.wantExit == 2 {
				if exit != 2 {
					t.Errorf("exit = %d, want 2\n%s\n%s", exit, stdout, stderr)
				}

				if got := len(server.Requests()); got != 0 {
					t.Errorf("the fixture saw %d request(s), want 0", got)
				}

				return
			}

			// An accepted pair changes the body, so the fixture refuses it
			// — that is fine and is not what is being measured. What is
			// measured is that the CLI got as far as sending, rather than
			// refusing the pair.
			if got := requestsTo(server, simulatePath); got != 1 {
				t.Errorf("simulate requests = %d, want 1: the pair should have been accepted\n%s\n%s",
					got, stdout, stderr)
			}
		})
	}
}

// recordedRequestBody reads a recording's request body, so the expectation
// and the fixture's matcher have one source.
func recordedRequestBody(t *testing.T, scenario, method, path string) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal([]byte(recordedRequestBodyRaw(t, scenario, method, path)), &out); err != nil {
		t.Fatalf("the recorded body for %s is not an object: %v", scenario, err)
	}

	return out
}

func recordedRequestBodyRaw(t *testing.T, scenario, method, path string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "recorded", scenario+".json"))
	if err != nil {
		t.Fatalf("read the recording %s: %v", scenario, err)
	}

	var recording struct {
		Interactions []struct {
			Request struct {
				Method string          `json:"method"`
				Path   string          `json:"path"`
				Body   json.RawMessage `json:"body"`
			} `json:"request"`
		} `json:"interactions"`
	}

	if err := json.Unmarshal(raw, &recording); err != nil {
		t.Fatalf("parse the recording %s: %v", scenario, err)
	}

	for _, i := range recording.Interactions {
		if i.Request.Method == method && i.Request.Path == path {
			return string(i.Request.Body)
		}
	}

	t.Fatalf("the recording %s has no %s %s", scenario, method, path)

	return ""
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
