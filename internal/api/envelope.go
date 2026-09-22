package api

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coba-ai/ferry-cli/internal/outcome"
)

// ErrEnvelopeShape is returned when a body is JSON but is not the seven-key
// error envelope.
//
// It is a distinct error and not a nil result because AC83 turns on the
// difference: "FERRY refused this, and here is the code" and "something
// answered, and it was not FERRY's error renderer" are different facts, and
// the second one is `pending` on a money path.
var ErrEnvelopeShape = errors.New("api: response body is not the FERRY error envelope")

// EnvelopeKeys is `Error.error.required` in `openapi.yaml`, in the document's
// order. AC17 pins the list to the document in both directions.
//
// All seven are always present, `null` where they do not apply, so a client
// never has to test for key presence as well as value. That is a property of
// the contract this decoder is entitled to rely on — and therefore one it must
// check, because a body missing a key did not come from FERRY's renderer.
var EnvelopeKeys = []string{
	"code",
	"message",
	"retriable",
	"retry_after_seconds",
	"details",
	"request_id",
	"docs_url",
}

// Envelope is a FERRY error.
type Envelope struct {
	Code              string
	Message           string
	Retriable         bool
	RetryAfterSeconds *int
	Details           map[string]any
	RequestID         string
	DocsURL           string
}

// Outcome converts to the decision table's input type. The table takes a plain
// struct so that `internal/outcome` imports nothing; this is the one place the
// two shapes meet.
func (e Envelope) Outcome() *outcome.Envelope {
	return &outcome.Envelope{Code: e.Code, Details: e.Details}
}

// DecodeEnvelope reads an error body.
//
// It requires all seven keys to be *present*, not merely non-empty: a `null`
// `details` is the contract's way of saying "no context applies", and
// rejecting it would reject most of the catalogue. Presence is checked against
// the decoded object rather than the struct, because `encoding/json` cannot
// tell an absent key from a zero value.
func DecodeEnvelope(body []byte) (Envelope, error) {
	var outer struct {
		Error map[string]json.RawMessage `json:"error"`
	}

	if err := json.Unmarshal(body, &outer); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrEnvelopeShape, err)
	}

	if outer.Error == nil {
		return Envelope{}, fmt.Errorf("%w: no `error` object", ErrEnvelopeShape)
	}

	for _, key := range EnvelopeKeys {
		if _, ok := outer.Error[key]; !ok {
			return Envelope{}, fmt.Errorf("%w: `error.%s` is absent", ErrEnvelopeShape, key)
		}
	}

	var inner struct {
		Code              string         `json:"code"`
		Message           string         `json:"message"`
		Retriable         bool           `json:"retriable"`
		RetryAfterSeconds *int           `json:"retry_after_seconds"`
		Details           map[string]any `json:"details"`
		RequestID         string         `json:"request_id"`
		DocsURL           string         `json:"docs_url"`
	}

	raw, err := json.Marshal(outer.Error)
	if err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrEnvelopeShape, err)
	}

	if err := json.Unmarshal(raw, &inner); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrEnvelopeShape, err)
	}

	// An envelope with no `code` is one nothing can branch on, and branching
	// on the code is the whole contract (`openapi.yaml`, Error.code: "Branch
	// on this. Never on the status, which is shared, or the message, which
	// is prose"). Treat it as a shape failure so AC83's row catches it.
	if inner.Code == "" {
		return Envelope{}, fmt.Errorf("%w: `error.code` is empty", ErrEnvelopeShape)
	}

	return Envelope{
		Code:              inner.Code,
		Message:           inner.Message,
		Retriable:         inner.Retriable,
		RetryAfterSeconds: inner.RetryAfterSeconds,
		Details:           inner.Details,
		RequestID:         inner.RequestID,
		DocsURL:           inner.DocsURL,
	}, nil
}
