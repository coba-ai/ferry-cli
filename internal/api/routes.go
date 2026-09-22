// Package api is the FERRY HTTP client: one request, one answer, and the
// facts `internal/outcome` needs to say what the answer means.
//
// It decides nothing. Every branch on what an answer *means* lives in the
// decision table; this package's job is to send exactly what it was given,
// read exactly what came back, and lose nothing on the way — including the
// difference between "the connection was never made" and "the request was on
// the wire when it failed", which no error string reliably carries.
package api

import (
	"net/http"

	"github.com/coba-ai/ferry-cli/internal/outcome"
)

// Route is one operation, pinned to `openapi.yaml`.
//
// Path is the templated path as the contract writes it, `{id}` and all, so the
// pin compares the same strings the document does; Expand fills it.
type Route struct {
	Op        outcome.Operation
	OpenAPIID string
	Method    string
	Path      string

	// Idempotent is whether `openapi.yaml` declares the `IdempotencyKey`
	// parameter for this operation. AC16 holds this column to the document
	// in both directions, which is the direction that matters: a money
	// route that stops sending the key is a route whose retries execute
	// twice.
	Idempotent bool
}

// Routes is every operation this CLI performs.
//
// It excludes `POST /oms/webhooks/{locator}`, which is FERRY's ingress from
// its upstream and not a client surface, and `GET /v1/{path}`, which is the
// catch-all that answers `NOT_FOUND` for anything unrouted. AC25 pins this set
// to the document minus exactly those two.
var Routes = []Route{
	{Op: outcome.OpGetMe, OpenAPIID: "getMe", Method: http.MethodGet, Path: "/v1/me"},
	{Op: outcome.OpListAPIKeys, OpenAPIID: "listApiKeys", Method: http.MethodGet, Path: "/v1/api_keys"},
	{Op: outcome.OpCreateAPIKey, OpenAPIID: "createApiKey", Method: http.MethodPost, Path: "/v1/api_keys"},
	{Op: outcome.OpGetAPIKey, OpenAPIID: "getApiKey", Method: http.MethodGet, Path: "/v1/api_keys/{id}"},
	{Op: outcome.OpRevokeAPIKey, OpenAPIID: "revokeApiKey", Method: http.MethodPost, Path: "/v1/api_keys/{id}/revoke"},
	{Op: outcome.OpListCorridors, OpenAPIID: "listCorridors", Method: http.MethodGet, Path: "/v1/corridors"},
	{Op: outcome.OpGetCorridor, OpenAPIID: "getCorridor", Method: http.MethodGet, Path: "/v1/corridors/{id}"},
	{
		Op: outcome.OpSimulateTransfer, OpenAPIID: "simulateTransfer",
		Method: http.MethodPost, Path: "/v1/transfers/simulate", Idempotent: true,
	},
	{
		Op: outcome.OpExecuteTransfer, OpenAPIID: "executeTransfer",
		Method: http.MethodPost, Path: "/v1/transfers", Idempotent: true,
	},
	{Op: outcome.OpGetCommand, OpenAPIID: "getCommand", Method: http.MethodGet, Path: "/v1/commands/{id}"},
}

// ExcludedPaths are the two paths AC25 subtracts from the document before
// comparing. Named here rather than written into the test so that the reason
// for each exclusion lives next to the table it excludes them from.
var ExcludedPaths = map[string]string{
	"/oms/webhooks/{locator}": "FERRY's ingress from its upstream; no client calls it",
	"/v1/{path}":              "the catch-all that answers NOT_FOUND for anything unrouted",
}

// Scopes is the `CreateApiKeyRequest.scopes` enum, in the document's order.
//
// `keys:manage` is deliberately absent: it is control-plane only and cannot be
// granted to an API key, which is what keeps revocation meaningful. AC25 pins
// this list to the document.
var Scopes = []string{
	"read",
	"compliance:read",
	"write",
	"destinations:manage",
	"money:simulate",
	"money:execute",
}

// RouteFor finds the route for an operation.
func RouteFor(op outcome.Operation) (Route, bool) {
	for _, r := range Routes {
		if r.Op == op {
			return r, true
		}
	}

	return Route{}, false
}
