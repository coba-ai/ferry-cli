package api_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kurenn/ferry-cli/internal/api"
)

// AC17. The decoder requires all seven keys.
//
// The reason to be strict is AC83: a body that is missing a key did not come
// from FERRY's error renderer, and an answer that did not come from FERRY says
// nothing about what FERRY did. A lenient decoder turns that into a confident
// `refused_fix` — "nothing was sent" — on the strength of a body some
// middlebox wrote.

func fullEnvelope() map[string]any {
	return map[string]any{
		"code":                "VALIDATION_FAILED",
		"message":             "the request was not valid",
		"retriable":           false,
		"retry_after_seconds": nil,
		"details":             map[string]any{"fields": []any{"amount.value"}},
		"request_id":          "req_01",
		"docs_url":            "https://example.test/errors#validation_failed",
	}
}

func envelopeBody(t *testing.T, inner map[string]any) []byte {
	t.Helper()

	b, err := json.Marshal(map[string]any{"error": inner})
	if err != nil {
		t.Fatalf("building an envelope: %v", err)
	}

	return b
}

func TestDecodeEnvelopeReadsTheSevenKeys(t *testing.T) {
	got, err := api.DecodeEnvelope(envelopeBody(t, fullEnvelope()))
	if err != nil {
		t.Fatalf("a complete envelope was rejected: %v", err)
	}

	if got.Code != "VALIDATION_FAILED" {
		t.Errorf("code: got %q", got.Code)
	}

	if got.Message != "the request was not valid" {
		t.Errorf("message: got %q", got.Message)
	}

	if got.Retriable {
		t.Error("retriable: got true")
	}

	if got.RetryAfterSeconds != nil {
		t.Errorf("retry_after_seconds: got %v, want nil for a null", *got.RetryAfterSeconds)
	}

	if got.RequestID != "req_01" {
		t.Errorf("request_id: got %q", got.RequestID)
	}

	if got.DocsURL == "" {
		t.Error("docs_url was dropped")
	}

	if _, ok := got.Details["fields"]; !ok {
		t.Errorf("details: got %v", got.Details)
	}
}

// Each of the seven, one at a time. A single example removing one key would
// prove the decoder checks *a* key; this proves it checks all of them, which
// is the claim AC17 makes.
func TestEveryEnvelopeKeyIsRequired(t *testing.T) {
	if len(api.EnvelopeKeys) != 7 {
		t.Fatalf("expected seven keys, got %d", len(api.EnvelopeKeys))
	}

	for _, key := range api.EnvelopeKeys {
		t.Run("without "+key, func(t *testing.T) {
			inner := fullEnvelope()
			delete(inner, key)

			_, err := api.DecodeEnvelope(envelopeBody(t, inner))
			if !errors.Is(err, api.ErrEnvelopeShape) {
				t.Errorf("a body with no `%s` decoded: %v", key, err)
			}

			if err != nil && !strings.Contains(err.Error(), key) {
				t.Errorf("the error does not name the missing key: %v", err)
			}
		})
	}
}

// A `null` value is not an absent key. The contract is explicit that all seven
// are always present, "`null` where they do not apply, so a client never has
// to test for key presence as well as value" — so rejecting nulls would reject
// most of the catalogue.
func TestNullValuesAreAccepted(t *testing.T) {
	inner := fullEnvelope()
	inner["retry_after_seconds"] = nil
	inner["details"] = nil
	inner["request_id"] = nil

	got, err := api.DecodeEnvelope(envelopeBody(t, inner))
	if err != nil {
		t.Fatalf("an envelope with nulls was rejected: %v", err)
	}

	if got.Details != nil {
		t.Errorf("a null `details` became %v", got.Details)
	}

	if got.RequestID != "" {
		t.Errorf("a null `request_id` became %q", got.RequestID)
	}
}

func TestNonEnvelopeBodiesAreRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"an HTML error page from a proxy", "<html><body>502 Bad Gateway</body></html>"},
		{"an empty body", ""},
		{"a JSON document with no error object", `{"message":"nope"}`},
		{"a null error", `{"error":null}`},
		{"an error that is not an object", `{"error":"nope"}`},
		{"a JSON array", `[1,2,3]`},
		// A code nothing can branch on. The contract's own instruction is
		// "branch on this", so an envelope without one is not usable as an
		// envelope.
		{"an empty code", `{"error":{"code":"","message":"m","retriable":false,"retry_after_seconds":null,"details":null,"request_id":null,"docs_url":"d"}}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := api.DecodeEnvelope([]byte(c.body)); !errors.Is(err, api.ErrEnvelopeShape) {
				t.Errorf("decoded: %v", err)
			}
		})
	}
}

// A non-null `retry_after_seconds` survives as a number, because §5.8 waits on
// it when the header is absent.
func TestRetryAfterSecondsIsRead(t *testing.T) {
	inner := fullEnvelope()
	inner["retry_after_seconds"] = 12

	got, err := api.DecodeEnvelope(envelopeBody(t, inner))
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}

	if got.RetryAfterSeconds == nil || *got.RetryAfterSeconds != 12 {
		t.Errorf("retry_after_seconds: got %v", got.RetryAfterSeconds)
	}
}
