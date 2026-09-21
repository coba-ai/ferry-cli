package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/kurenn/ferry/cli/internal/creds"
)

// StatusUnrecorded is what the fixture answers a request no loaded recording
// matches (AC29).
//
// It is outside the range any FERRY endpoint can return, and `api.Retryable`
// exempts it from the 5xx retry rules by name (CRITIQUE N5) — a fixture saying
// "nothing here matches what you sent" is a test defect, and retrying it turns
// a clear failure into a slow one.
const StatusUnrecorded = 599

// UnrecordedHeader carries `<METHOD> <request-target>` on that answer, so a
// test that gets one can say which request went unmatched without reading the
// body (AC29).
const UnrecordedHeader = "X-Fixture-Unrecorded"

// Credential classes, in the recorder's vocabulary
// (`spec/support/cli/recorder.rb`). They are not `creds.Class`'s spelling:
// `creds` says "pat" where the recordings say "personal_access_token", and the
// recordings are what this package matches against.
const (
	classAPIKey = "api_key"
	classPAT    = "personal_access_token"
)

// TB is the part of *testing.T this package uses. Declared here rather than
// taking testing.TB so that no non-test binary linking this package pulls in
// the testing flag set.
type TB interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
}

// Request is one request the server received, logged whether or not it
// matched (AC31).
type Request struct {
	// At is when the handler received it — before any Hold blocks it, so
	// two entries' spacing is the spacing of the client's sends.
	At time.Time

	Method string

	// Path is the escaped request target's path: `/v1/corridors/walletCrypto-%3EbankUs`
	// and `/v1/corridors/walletCrypto->bankUs` are different strings here,
	// which is what AC40 is about.
	Path string

	// Query is the parsed query, and RawQuery the string as it arrived.
	// Both are kept: the match compares values, because the client's
	// url.Values.Encode sorts by key and the recorder wrote the target in
	// the order a human typed it, and a test that wants to say something
	// about the bytes needs the bytes.
	Query    url.Values
	RawQuery string

	Header http.Header

	// Body is the exact bytes the client sent, and SHA256 their digest.
	// Byte identity between two requests — a resume resending what it
	// recorded (AC45, AC80) — is asserted here, Go against Go, and never
	// against a recording: §5.10 is explicit that a recording is
	// authoritative for a request's semantic content and not for its bytes.
	Body   []byte
	SHA256 string

	// Scenario and Interaction name the recording that answered, and are
	// "" and -1 for a request that matched nothing.
	Scenario    string
	Interaction int

	// Status is what the server answered.
	Status int
}

// IdempotencyKey is the key the client sent, or "".
func (r Request) IdempotencyKey() string { return r.Header.Get("Idempotency-Key") }

// Matched reports whether a recording answered this request.
func (r Request) Matched() bool { return r.Scenario != "" }

// Server is an httptest.Server that answers from recordings.
type Server struct {
	t    TB
	set  *Set
	http *httptest.Server

	mu     sync.Mutex
	tracks []*track
	log    []Request
	holds  map[string]*hold
}

// track is one named scenario and how far through it the server is. A
// scenario named twice gets two tracks, which is how a test drives "send,
// then resend and get the replay".
type track struct {
	scenario string
	rec      *Recording
	next     int
}

type hold struct {
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

// New starts a server that will answer from the named recordings, in the order
// each recording's interactions were recorded (AC31).
//
// Naming no recordings is allowed and useful: a test that asserts the CLI sent
// nothing wants a server that would have answered 599 to anything.
func New(t TB, scenarios ...string) *Server {
	t.Helper()

	set, err := Recorded()
	if err != nil {
		t.Fatalf("fixture: %v", err)

		return nil
	}

	return NewFromSet(t, set, scenarios...)
}

// NewFromSet is New against a directory already loaded. Load refuses a
// directory that disagrees with its manifest, so there is no way to reach a
// server over unchecked recordings.
func NewFromSet(t TB, set *Set, scenarios ...string) *Server {
	t.Helper()

	s := &Server{t: t, set: set, holds: map[string]*hold{}}

	for _, name := range scenarios {
		rec, ok := set.Recordings[name]
		if !ok {
			t.Fatalf("fixture: no recording named %q in %s", name, set.Dir)

			return nil
		}

		s.tracks = append(s.tracks, &track{scenario: name, rec: rec})
	}

	s.http = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)

	return s
}

// URL is the base URL a profile's `api_url` is set to.
func (s *Server) URL() string { return s.http.URL }

// Close stops the server. Registered with t.Cleanup by New.
func (s *Server) Close() { s.http.Close() }

// Requests is every request received, in order (AC31).
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Request, len(s.log))
	copy(out, s.log)

	return out
}

// Unmatched counts requests no recording answered.
func (s *Server) Unmatched() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0

	for _, entry := range s.log {
		if !entry.Matched() {
			n++
		}
	}

	return n
}

// Remaining names the scenarios with interactions still unplayed, each with
// how many are left.
//
// A test that drove a sequence and stopped early is a test whose subject did
// less than the test's name says; this is how it can tell.
func (s *Server) Remaining() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := map[string]int{}

	for _, tr := range s.tracks {
		if left := len(tr.rec.Interactions) - tr.next; left > 0 {
			out[tr.scenario] += left
		}
	}

	return out
}

// Hold blocks the next request this scenario answers until release is called
// (AC91).
//
// It returns both halves because a release function alone cannot be used
// without a race: the test has to know the request is on the wire before it
// delivers a signal to the client, and polling for that cannot witness a
// window shorter than the poll. `arrived` closes inside the handler, after the
// request is logged and before a byte of the answer is written; `release` is
// safe to call more than once and from any goroutine.
func (s *Server) Hold(scenario string) (arrived <-chan struct{}, release func()) {
	s.t.Helper()

	known := false

	s.mu.Lock()

	for _, tr := range s.tracks {
		if tr.scenario == scenario {
			known = true

			break
		}
	}

	if !known {
		s.mu.Unlock()
		s.t.Fatalf("fixture: cannot hold %q; this server does not serve it", scenario)

		return nil, func() {}
	}

	if _, ok := s.holds[scenario]; ok {
		s.mu.Unlock()
		s.t.Fatalf("fixture: %q is already held", scenario)

		return nil, func() {}
	}

	h := &hold{arrived: make(chan struct{}), release: make(chan struct{})}
	s.holds[scenario] = h
	s.mu.Unlock()

	return h.arrived, func() { h.once.Do(func() { close(h.release) }) }
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	received := time.Now()

	body, readErr := io.ReadAll(r.Body)

	digest := sha256.Sum256(body)

	entry := Request{
		At:          received,
		Method:      r.Method,
		Path:        r.URL.EscapedPath(),
		Query:       r.URL.Query(),
		RawQuery:    r.URL.RawQuery,
		Header:      r.Header.Clone(),
		Body:        body,
		SHA256:      hex.EncodeToString(digest[:]),
		Interaction: -1,
	}

	s.mu.Lock()

	var (
		chosen  *track
		matched *Interaction
		reasons []string
	)

	if readErr == nil {
		chosen, matched, reasons = s.matchLocked(entry)
	} else {
		reasons = []string{fmt.Sprintf("the request body could not be read: %v", readErr)}
	}

	var held *hold

	if matched != nil {
		entry.Scenario = chosen.scenario
		entry.Interaction = chosen.next
		entry.Status = matched.Response.Status
		chosen.next++

		if h, ok := s.holds[chosen.scenario]; ok {
			held = h

			delete(s.holds, chosen.scenario)
		}
	} else {
		entry.Status = StatusUnrecorded
	}

	s.log = append(s.log, entry)

	s.mu.Unlock()

	if held != nil {
		close(held.arrived)
		<-held.release
	}

	if matched == nil {
		s.writeUnrecorded(w, entry, reasons)

		return
	}

	writeRecorded(w, matched.Response)
}

// matchLocked finds the recording that answers this request (AC30).
//
// The candidates are the *next unplayed* interaction of each loaded scenario,
// in the order the test named them. A request that matches a later interaction
// of a scenario but not its next one is unrecorded, which is what makes a
// sequence a sequence: an out-of-order poll is a defect in the client, not a
// hint to the server.
func (s *Server) matchLocked(req Request) (*track, *Interaction, []string) {
	var reasons []string

	for _, tr := range s.tracks {
		if tr.next >= len(tr.rec.Interactions) {
			reasons = append(reasons, fmt.Sprintf("%s: all %d interactions have been played",
				tr.scenario, len(tr.rec.Interactions)))

			continue
		}

		interaction := &tr.rec.Interactions[tr.next]

		if why := mismatch(interaction.Request, req); why != "" {
			reasons = append(reasons, fmt.Sprintf("%s[%d]: %s", tr.scenario, tr.next, why))

			continue
		}

		return tr, interaction, nil
	}

	return nil, nil, reasons
}

// mismatch says why a recorded request does not answer this one, or "".
//
// The axes are AC30's — method, path, `Idempotency-Key` presence, structural
// equality of the body — plus the credential class, which the recordings carry
// and the API enforces before anything else (§1.2.1). Matching on the class is
// A331: ignoring it would let a test send a PAT to `POST /v1/transfers` and be
// answered with the 201 that FERRY reserves for an API key.
func mismatch(recorded RecordedRequest, got Request) string {
	if !strings.EqualFold(recorded.Method, got.Method) {
		return fmt.Sprintf("method is %s, recorded %s", got.Method, recorded.Method)
	}

	recordedPath, recordedQuery := splitTarget(recorded.Path)

	if got.Path != recordedPath {
		return fmt.Sprintf("path is %q, recorded %q", got.Path, recordedPath)
	}

	if !sameQuery(recordedQuery, got.Query) {
		return fmt.Sprintf("query is %q, recorded %q", got.Query.Encode(), recordedQuery.Encode())
	}

	keyPresent := got.IdempotencyKey() != ""
	if keyPresent != recorded.Headers.IdempotencyKeyPresent {
		if recorded.Headers.IdempotencyKeyPresent {
			return "no Idempotency-Key; the recording was made with one"
		}

		return "an Idempotency-Key was sent; the recording was made without one"
	}

	if why := classMismatch(recorded.Headers.CredentialClass, got.Header.Get("Authorization")); why != "" {
		return why
	}

	return bodyMismatch(recorded.Body, got.Body)
}

func splitTarget(target string) (string, url.Values) {
	path, rawQuery, found := strings.Cut(target, "?")
	if !found {
		return path, url.Values{}
	}

	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return path, url.Values{}
	}

	return path, values
}

// sameQuery compares parsed query values rather than the raw string: the
// client builds its query with url.Values.Encode, which sorts by key, and the
// recorder wrote the target in the order a human typed it —
// `?limit=2&cursor=…` against `?cursor=…&limit=2`. The difference is not
// semantic and neither side should have to know the other's ordering.
func sameQuery(recorded, got url.Values) bool {
	if len(recorded) != len(got) {
		return false
	}

	for key, want := range recorded {
		have, ok := got[key]
		if !ok || !reflect.DeepEqual(want, have) {
			return false
		}
	}

	return true
}

// classMismatch derives the credential class from the token's shape, which is
// what `Ferry::Tokens` does and what `creds.Classify` already encodes (§1.2.2).
func classMismatch(recorded *string, authorization string) string {
	token := strings.TrimPrefix(authorization, "Bearer ")
	if authorization == "" {
		token = ""
	}

	var got string

	switch {
	case token == "":
		got = ""
	default:
		class, err := creds.Classify(token)
		switch {
		case err != nil:
			got = "a token of no recognised shape"
		case class == creds.ClassAPIKey:
			got = classAPIKey
		case class == creds.ClassPAT:
			got = classPAT
		default:
			got = string(class)
		}
	}

	want := ""
	if recorded != nil {
		want = *recorded
	}

	if got == want {
		return ""
	}

	return fmt.Sprintf("credential class is %s, recorded %s", describeClass(got), describeClass(want))
}

func describeClass(class string) string {
	if class == "" {
		return "absent"
	}

	return class
}

// bodyMismatch is AC30's structural comparison.
//
// Key order and whitespace are not semantic to the API and are not compared;
// an extra key, a missing key or a different value is. A recorded body that is
// a JSON *string* is a request the recorder sent verbatim — the
// `415 UNSUPPORTED_MEDIA_TYPE` scenario sends `name=a` — and is compared byte
// for byte, because for that request the bytes are the whole point.
func bodyMismatch(recorded json.RawMessage, got []byte) string {
	if len(recorded) == 0 || string(recorded) == "null" {
		if len(got) > 0 {
			return fmt.Sprintf("a body was sent (%s); the recording has none", brief(string(got)))
		}

		return ""
	}

	var want any
	if err := json.Unmarshal(recorded, &want); err != nil {
		return fmt.Sprintf("the recorded body is not JSON: %v", err)
	}

	if literal, ok := want.(string); ok {
		if string(got) != literal {
			return fmt.Sprintf("body is %s, recorded the verbatim bytes %s", brief(string(got)), brief(literal))
		}

		return ""
	}

	if len(got) == 0 {
		return "no body was sent; the recording has one"
	}

	var have any
	if err := json.Unmarshal(got, &have); err != nil {
		return fmt.Sprintf("body is not JSON: %v", err)
	}

	if !reflect.DeepEqual(want, have) {
		return fmt.Sprintf("body is structurally different: sent %s, recorded %s", brief(have), brief(want))
	}

	return ""
}

// writeRecorded answers with exactly what was recorded (AC32).
//
// Every header comes from the recording. The server invents no
// `Ferry-Command-Id`, no `Retry-After` and no `Idempotency-Replayed`, and it
// does not echo the request's `Idempotency-Key` either: a client that reads a
// command id the API never sent is a client whose polling works only against
// this fixture. `Content-Type` is the one header set here, because it is the
// transport's statement about the bytes rather than one of FERRY's answers,
// and Go would otherwise sniff one.
func writeRecorded(w http.ResponseWriter, resp RecordedResponse) {
	body := compactJSON(resp.Body)

	for _, name := range sortedKeys(resp.Headers) {
		w.Header().Set(http.CanonicalHeaderKey(name), resp.Headers[name])
	}

	if len(body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}

	w.WriteHeader(resp.Status)

	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// compactJSON serves the recorded document without the pretty-printing the
// file is stored with. The bytes are the recording's, key order and all; only
// the indentation the committed file carries for review is removed.
func compactJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return raw
	}

	return out.Bytes()
}

func (s *Server) writeUnrecorded(w http.ResponseWriter, req Request, reasons []string) {
	target := req.Path
	if encoded := req.Query.Encode(); encoded != "" {
		target += "?" + encoded
	}

	w.Header().Set(UnrecordedHeader, req.Method+" "+target)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(StatusUnrecorded)

	sent := string(req.Body)
	if len(sent) > 2048 {
		sent = sent[:2048] + "…"
	}

	payload := struct {
		Unrecorded string   `json:"unrecorded"`
		Body       string   `json:"body"`
		Candidates []string `json:"candidates"`
	}{
		Unrecorded: req.Method + " " + target,
		Body:       sent,
		Candidates: reasons,
	}

	if len(payload.Candidates) == 0 {
		payload.Candidates = []string{"this server was given no recordings"}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}

	_, _ = w.Write(encoded)
}
