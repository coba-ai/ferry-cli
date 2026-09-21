package api

import (
	"net/http"
	"strconv"
)

// MetaHeaders is `components.headers` in `openapi.yaml`, mapped to the field
// each one is read into. AC25 pins the key set to the document in both
// directions, so a header FERRY starts sending cannot be silently dropped on
// the floor here.
var MetaHeaders = map[string]string{
	"Ferry-Command-Id":     "CommandID",
	"Ferry-Environment":    "Environment",
	"Idempotency-Key":      "IdempotencyKey",
	"Idempotency-Replayed": "Replayed",
	"Retry-After":          "RetryAfter",
	"WWW-Authenticate":     "WWWAuthenticate",
}

// Meta is what the response headers said.
type Meta struct {
	Status int

	// CommandID is `Ferry-Command-Id`. Present on every answer that has a
	// command — including the 409 in-progress, the 503 `UPSTREAM_BUSY` and
	// the 403 policy refusals, whose envelopes carry no `poll` link, so this
	// is how the command is found.
	CommandID string

	// Environment is `Ferry-Environment`: which environment the credential
	// actually resolved to.
	Environment string

	IdempotencyKey string

	// Replayed is `Idempotency-Replayed: true` — a stored outcome rather
	// than a fresh one.
	Replayed bool

	// RetryAfter is nil when the header is absent, and that is not the same
	// as zero: `max(retry_after_seconds, 1)` on an absent header would wait
	// a second, while zero would be read as "immediately" by anything that
	// treats the field as a number. AC18 holds it to nil.
	RetryAfter *int

	WWWAuthenticate string

	// EnvironmentMismatch is set when the answer's `Ferry-Environment`
	// disagrees with the environment the credential was expected to resolve
	// to. C9: a caller who believes they are on sandbox and is answered by
	// live has already moved real money by the time they read the body.
	EnvironmentMismatch bool

	// ExpectedEnvironment is what the caller said the credential was for.
	// Empty when the caller did not say, in which case no mismatch is
	// reported — an absent expectation is not a disagreement.
	ExpectedEnvironment string
}

// ReadMeta reads the six headers.
//
// expectEnvironment is the environment the credential is believed to belong
// to, or "" when the caller has no belief to check.
func ReadMeta(status int, h http.Header, expectEnvironment string) Meta {
	m := Meta{
		Status:              status,
		CommandID:           h.Get("Ferry-Command-Id"),
		Environment:         h.Get("Ferry-Environment"),
		IdempotencyKey:      h.Get("Idempotency-Key"),
		Replayed:            h.Get("Idempotency-Replayed") == "true",
		WWWAuthenticate:     h.Get("WWW-Authenticate"),
		ExpectedEnvironment: expectEnvironment,
	}

	if raw := h.Get("Retry-After"); raw != "" {
		// A `Retry-After` this CLI cannot parse stays nil rather than
		// becoming zero, for the reason on the field.
		if n, err := strconv.Atoi(raw); err == nil {
			m.RetryAfter = &n
		}
	}

	m.EnvironmentMismatch = expectEnvironment != "" &&
		m.Environment != "" &&
		m.Environment != expectEnvironment

	return m
}

// RetryDelaySeconds is §5.8's wait: `max(retry_after_seconds, 1)`.
//
// The floor is one second and not zero because a server that answers
// `Retry-After: 0` under load is asking for a hot loop, and because the nil
// case must not be spelled the same way as the zero case.
func (m Meta) RetryDelaySeconds() int {
	if m.RetryAfter == nil || *m.RetryAfter < 1 {
		return 1
	}

	return *m.RetryAfter
}
