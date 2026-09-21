// Package fixture serves FERRY's recorded interactions (U0,
// `testdata/recorded/`) to the CLI's Go tests.
//
// It is the only place a Go test meets a FERRY response body, and C13 is the
// reason it exists: every body it serves came out of the real Rack app, was
// asserted against a declared expectation before it was written (AC76), and is
// asserted against that same expectation again here before a single byte is
// served (AC77). A request no recording matches is answered `599` with
// `X-Fixture-Unrecorded` (AC29) rather than with something plausible, because a
// fixture that is more permissive than the API teaches every unit downstream a
// lesson the API will not honour — and the units downstream are the ones that
// move money.
//
// Nothing in this package writes to `testdata/recorded/`. It is U0's, and
// the `test` job fails on `git diff --exit-code` over it (A306).
package fixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Axes is the expectation vocabulary, exactly (AC77, A300).
//
// An entry must carry all seven and nothing else. Both directions are errors
// on purpose:
//
//   - An axis the loader does not decode is an assertion U0 made against the
//     Rack app that the fixture would silently drop — worse than never having
//     written it, because the manifest still reads as though it were enforced.
//   - A *missing* axis is the same failure wearing the other costume: it is
//     indistinguishable from "no constraint on this axis", so an expectation
//     could lose its teeth by deletion and stay green.
var Axes = []string{
	"status",
	"code",
	"headers_present",
	"headers_absent",
	"body_has",
	"body_lacks",
	"body_equals",
}

// Manifest is `testdata/recorded/MANIFEST.json`.
type Manifest struct {
	Scenarios   []ManifestEntry `json:"scenarios"`
	Unreachable []Unreachable   `json:"unreachable"`
}

// ManifestEntry is one recorded scenario: its file's digest and what U0
// asserted the app answered.
type ManifestEntry struct {
	Scenario string        `json:"scenario"`
	SHA256   string        `json:"sha256"`
	Expect   []Expectation `json:"expect"`

	// Note and Constructed are U0's prose about how a scenario was reached.
	// They are decoded rather than ignored so that the strict decode below
	// keeps its meaning: "unknown key" has to be a real signal, which it
	// cannot be if the loader has a habit of tolerating keys it knows about
	// but does not read.
	Note        string `json:"note,omitempty"`
	Constructed string `json:"constructed,omitempty"`
}

// Unreachable is a scenario §5.10 declares cannot be produced from the request
// path, with the reason. It has no file and no digest, and the loader holds it
// to that: an "unreachable" name with a recording on disk is a contradiction
// between the manifest's two halves.
type Unreachable struct {
	Scenario string `json:"scenario"`
	Reason   string `json:"reason"`
}

// Expectation is one interaction's declared answer (§5.10, A300, A301).
//
// `expect` is an array — one entry per interaction, not one per scenario
// (A301). Four scenarios are sequences, and for those a single object would
// collapse to whatever answered last.
type Expectation struct {
	// Status is the HTTP status. Always asserted; there is no "any status".
	Status int

	// Code is `error.code`. Nil means the body carries no error code at all,
	// which is an assertion and not an absence of one. A value wrapped in
	// slashes ("/^POLICY_/") is a regular expression; anything else is
	// compared literally.
	Code *string

	HeadersPresent []string
	HeadersAbsent  []string

	// BodyHas is dotted paths that must resolve to a non-null value;
	// BodyLacks is dotted paths that must be absent or null. The two are
	// exact complements, which is what the recorder's Ruby `dig` means and
	// what the recordings show: nineteen `body_lacks` paths are present-and-
	// null rather than missing.
	BodyHas   []string
	BodyLacks []string

	// BodyEquals is a path-to-value map (A300). It exists because presence
	// alone cannot tell `execute.key_reused.different_credential.409` from
	// `execute.key_reused.different_request.409`: same status, same code,
	// same field, and only the value says which scenario was recorded.
	BodyEquals map[string]any
}

// expectationJSON is the wire shape. Separate from Expectation so
// UnmarshalJSON can decode without recursing into itself.
type expectationJSON struct {
	Status         int            `json:"status"`
	Code           *string        `json:"code"`
	HeadersPresent []string       `json:"headers_present"`
	HeadersAbsent  []string       `json:"headers_absent"`
	BodyHas        []string       `json:"body_has"`
	BodyLacks      []string       `json:"body_lacks"`
	BodyEquals     map[string]any `json:"body_equals"`
}

// UnmarshalJSON decodes one expectation and holds its key set to Axes in both
// directions.
func (e *Expectation) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("expectation is not an object: %w", err)
	}

	known := make(map[string]bool, len(Axes))
	for _, axis := range Axes {
		known[axis] = true
	}

	var unknown, missing []string

	for key := range raw {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}

	for _, axis := range Axes {
		if _, ok := raw[axis]; !ok {
			missing = append(missing, axis)
		}
	}

	sort.Strings(unknown)

	switch {
	case len(unknown) > 0 && len(missing) > 0:
		return fmt.Errorf(
			"expectation axes %s are not decoded by this loader and axes %s are missing; "+
				"the vocabulary is exactly %s (AC77, A300)",
			strings.Join(unknown, ", "), strings.Join(missing, ", "), strings.Join(Axes, ", "))
	case len(unknown) > 0:
		return fmt.Errorf(
			"expectation axes %s are not decoded by this loader; an axis U0 asserted "+
				"Ruby-side that the fixture drops is an assertion nobody makes (AC77, A300). "+
				"The vocabulary is exactly %s",
			strings.Join(unknown, ", "), strings.Join(Axes, ", "))
	case len(missing) > 0:
		return fmt.Errorf(
			"expectation is missing axes %s; every entry carries all of %s, because an "+
				"omitted axis is indistinguishable from one that asserts nothing (AC77)",
			strings.Join(missing, ", "), strings.Join(Axes, ", "))
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var wire expectationJSON
	if err := dec.Decode(&wire); err != nil {
		return err
	}

	*e = Expectation{
		Status:         wire.Status,
		Code:           wire.Code,
		HeadersPresent: wire.HeadersPresent,
		HeadersAbsent:  wire.HeadersAbsent,
		BodyHas:        wire.BodyHas,
		BodyLacks:      wire.BodyLacks,
		BodyEquals:     wire.BodyEquals,
	}

	return nil
}

// Recording is one scenario file.
//
// It carries its own copy of `expect`; the loader asserts that copy equals the
// manifest's rather than reading one and trusting the other, because two
// documents on disk that are supposed to agree are two documents that can be
// edited apart.
type Recording struct {
	Scenario     string        `json:"scenario"`
	Expect       []Expectation `json:"expect"`
	Interactions []Interaction `json:"interactions"`
}

// Interaction is one request and the answer the app gave it.
type Interaction struct {
	Request  RecordedRequest  `json:"request"`
	Response RecordedResponse `json:"response"`
}

// RecordedRequest is the request U0 made, as the recorder wrote it: the
// request-target exactly as it went on the wire (`/v1/corridors/walletCrypto-%3EbankUs`,
// not what Rack decoded it to), and the body as a document rather than as
// bytes — §5.10 is explicit that the recording is authoritative for the body's
// semantic content and not for its serialisation.
type RecordedRequest struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Headers RequestHeaders  `json:"headers"`
	Body    json.RawMessage `json:"body"`
}

// RequestHeaders is the header subset the recorder captured.
type RequestHeaders struct {
	IdempotencyKeyPresent bool `json:"idempotency_key_present"`

	// CredentialClass is "api_key", "personal_access_token", or nil for the
	// one scenario that sends no credential at all.
	CredentialClass *string `json:"credential_class"`
}

// RecordedResponse is the answer.
type RecordedResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}
