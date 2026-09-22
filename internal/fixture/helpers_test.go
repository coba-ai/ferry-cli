package fixture_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/fixture"
)

// Every negative control in this package is a *corruption of the committed
// recordings*, not a hand-written document.
//
// Two reasons. C13: a hand-written FERRY body is a second authority, and a
// loader test written against one proves the loader agrees with whoever typed
// it. And a corruption is the failure that actually threatens this directory —
// a file edited, a file added, a digest left behind, an expectation weakened —
// so the control's subject is the real risk rather than a model of it.

// copyRecordings is a byte copy of `testdata/recorded/` into a temporary
// directory. The original is never opened for writing anywhere in this
// package; `git diff --exit-code testdata/recorded` (A306) is part of the
// gate.
func copyRecordings(t *testing.T) string {
	t.Helper()

	src := fixture.RecordedDir()
	dst := t.TempDir()

	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}

	if len(entries) == 0 {
		t.Fatalf("%s is empty", src)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("%s contains a directory, %s", src, entry.Name())
		}

		raw, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}

		if err := os.WriteFile(filepath.Join(dst, entry.Name()), raw, 0o600); err != nil {
			t.Fatalf("writing %s: %v", entry.Name(), err)
		}
	}

	return dst
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}

	return out
}

func writeJSON(t *testing.T, path string, document map[string]any) {
	t.Helper()

	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}

	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// editManifest rewrites the manifest in a copied directory. The recordings'
// digests are unaffected, so a test that edits only the manifest is testing
// the manifest side on purpose.
func editManifest(t *testing.T, dir string, edit func(manifest map[string]any)) {
	t.Helper()

	path := filepath.Join(dir, fixture.ManifestName)
	manifest := readJSON(t, path)
	edit(manifest)
	writeJSON(t, path, manifest)
}

func manifestEntry(t *testing.T, manifest map[string]any, scenario string) map[string]any {
	t.Helper()

	scenarios, ok := manifest["scenarios"].([]any)
	if !ok {
		t.Fatalf("manifest has no scenarios list")
	}

	for _, raw := range scenarios {
		entry, ok := raw.(map[string]any)
		if ok && entry["scenario"] == scenario {
			return entry
		}
	}

	t.Fatalf("the manifest does not name %s", scenario)

	return nil
}

// editExpect changes one scenario's expectation in *both* places it is
// written — the manifest and the recording — and refreshes the digest, so
// that the only thing left disagreeing is the expectation against the
// recorded response. Without that, every axis test would go red on "the file
// and the manifest differ" and prove nothing about the axis.
func editExpect(t *testing.T, dir, scenario string, index int, edit func(expect map[string]any)) {
	t.Helper()

	recordingPath := filepath.Join(dir, scenario+".json")
	recording := readJSON(t, recordingPath)

	recordedExpect, ok := recording["expect"].([]any)
	if !ok || index >= len(recordedExpect) {
		t.Fatalf("%s has no expect[%d]", scenario, index)
	}

	entry, ok := recordedExpect[index].(map[string]any)
	if !ok {
		t.Fatalf("%s expect[%d] is not an object", scenario, index)
	}

	edit(entry)
	writeJSON(t, recordingPath, recording)

	editManifest(t, dir, func(manifest map[string]any) {
		manifestEntry(t, manifest, scenario)["expect"] = recordedExpect
		manifestEntry(t, manifest, scenario)["sha256"] = digestOf(t, recordingPath)
	})
}

// dropExpectEntry removes one entry from a scenario's expect array in both
// places, which is A301's count check and nothing else.
func dropExpectEntry(t *testing.T, dir, scenario string, index int) {
	t.Helper()

	recordingPath := filepath.Join(dir, scenario+".json")
	recording := readJSON(t, recordingPath)

	recordedExpect, ok := recording["expect"].([]any)
	if !ok || index >= len(recordedExpect) {
		t.Fatalf("%s has no expect[%d]", scenario, index)
	}

	trimmed := append(append([]any{}, recordedExpect[:index]...), recordedExpect[index+1:]...)
	recording["expect"] = trimmed
	writeJSON(t, recordingPath, recording)

	editManifest(t, dir, func(manifest map[string]any) {
		manifestEntry(t, manifest, scenario)["expect"] = trimmed
		manifestEntry(t, manifest, scenario)["sha256"] = digestOf(t, recordingPath)
	})
}

func digestOf(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:])
}

// refuses asserts Load rejects a directory and that the reason it gives names
// what the test broke. A loader that refuses everything for one reason is not
// enforcing the axis the test is about.
func refuses(t *testing.T, dir string, wants ...string) {
	t.Helper()

	set, err := fixture.Load(dir)
	if err == nil {
		t.Fatalf("Load accepted a directory it should have refused (%d scenarios)", len(set.Order))
	}

	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load refused for the wrong reason.\nwant substring: %s\ngot: %v", want, err)
		}
	}
}

func rejects(t *testing.T, err error, unwanted ...string) {
	t.Helper()

	for _, no := range unwanted {
		if strings.Contains(err.Error(), no) {
			t.Errorf("the refusal mentions %q, which this test did not break: %v", no, err)
		}
	}
}

// The tokens below are shapes, not secrets: `Ferry::Tokens` fixes the two
// regexes (§1.2.2) and `creds.Classify` reads them, so a request has to carry
// a well-formed token of the right class to reach the recording that was made
// with one.
const (
	apiKeyToken = "ferry_sk_sandbox_" + fortyThree
	patToken    = "ferry_pat_" + fortyThree
	fortyThree  = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
)

func tokenFor(class *string) string {
	if class == nil {
		return ""
	}

	switch *class {
	case "api_key":
		return apiKeyToken
	case "personal_access_token":
		return patToken
	default:
		return ""
	}
}

// replay builds the request the recorder made, as a Go client would send it.
//
// The body is re-serialised from the decoded document rather than copied: Go's
// `encoding/json` sorts map keys and Ruby's `JSON.generate` does not, so every
// request this helper sends is in a different byte order from the recording it
// has to match. That is AC30's point — the match is structural — and it means
// this helper cannot pass by accident.
func replay(t *testing.T, base string, recorded fixture.RecordedRequest, key string) *http.Response {
	t.Helper()

	body, contentType := replayBody(t, recorded.Body)

	req, err := http.NewRequest(recorded.Method, base+recorded.Path, body)
	if err != nil {
		t.Fatalf("building %s %s: %v", recorded.Method, recorded.Path, err)
	}

	if token := tokenFor(recorded.Headers.CredentialClass); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	if recorded.Headers.IdempotencyKeyPresent {
		if key == "" {
			key = "cli-test-key-1"
		}

		req.Header.Set("Idempotency-Key", key)
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sending %s %s: %v", recorded.Method, recorded.Path, err)
	}

	return resp
}

func replayBody(t *testing.T, recorded json.RawMessage) (io.Reader, string) {
	t.Helper()

	if len(recorded) == 0 || string(recorded) == "null" {
		return nil, ""
	}

	var decoded any
	if err := json.Unmarshal(recorded, &decoded); err != nil {
		t.Fatalf("the recorded request body is not JSON: %v", err)
	}

	if literal, ok := decoded.(string); ok {
		return strings.NewReader(literal), "application/x-www-form-urlencoded"
	}

	reserialised, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-serialising the recorded request body: %v", err)
	}

	return bytes.NewReader(reserialised), "application/json"
}

// post sends a money POST with an explicit body, for the tests that perturb
// one key of it.
func post(t *testing.T, base, path string, body []byte, key string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building POST %s: %v", path, err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKeyToken)
	req.Header.Set("Content-Type", "application/json")

	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sending POST %s: %v", path, err)
	}

	return resp
}

func get(t *testing.T, base, path, token, key string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("building GET %s: %v", path, err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sending GET %s: %v", path, err)
	}

	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()

	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}

	return raw
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()

	raw := readBody(t, resp)

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the response body is not a JSON object: %v\n%s", err, raw)
	}

	return out
}

// recordingsOf reads the committed set once for the tests that iterate it.
func recordingsOf(t *testing.T) *fixture.Set {
	t.Helper()

	set, err := fixture.Recorded()
	if err != nil {
		t.Fatalf("loading the committed recordings: %v", err)
	}

	return set
}

func recordingFor(t *testing.T, set *fixture.Set, scenario string) *fixture.Recording {
	t.Helper()

	rec, ok := set.Recordings[scenario]
	if !ok {
		t.Fatalf("no recording named %s", scenario)
	}

	return rec
}

// mutate returns a copy of a decoded JSON document with one path changed, so
// a test can say "the recorded body with one key different" without writing a
// body.
func mutate(t *testing.T, raw json.RawMessage, edit func(document map[string]any)) []byte {
	t.Helper()

	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("the recorded body is not an object: %v", err)
	}

	edit(document)

	out, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("re-serialising: %v", err)
	}

	return out
}
