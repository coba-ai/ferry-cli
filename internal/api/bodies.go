package api

import "encoding/json"

// The request bodies, hand-written and pinned field-for-field to
// `openapi.yaml` (WD1, §5.12).
//
// Every one of these schemas is `additionalProperties: false`, and FERRY
// refuses an unrecognised top-level key rather than ignoring it — "a caller
// who misspells `sponsor_gas` and is answered `201` has been told FERRY
// honoured something it dropped". So a field this CLI can set that the
// contract does not declare is not a harmless extra; it is a request that will
// be refused, discovered at the wire instead of at the build.
//
// The `Fields` lists below are the "builders' declared sets" AC25 pins. They
// are held to the struct tags in one direction and to the document in the
// other, so neither the struct nor the document can drift alone.

// CreateAPIKeyRequest is `CreateApiKeyRequest`.
type CreateAPIKeyRequest struct {
	Name        string   `json:"name"`
	Environment string   `json:"environment"`
	Scopes      []string `json:"scopes"`
	// ExpiresAt is optional: omitted, the key lasts the maximum its
	// environment and scopes allow.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// CreateAPIKeyFields is the declared property set.
var CreateAPIKeyFields = []string{"name", "environment", "scopes", "expires_at"}

// RevokeAPIKeyRequest is `RevokeApiKeyRequest`.
type RevokeAPIKeyRequest struct {
	Reason string `json:"reason,omitempty"`
}

// RevokeAPIKeyFields is the declared property set.
var RevokeAPIKeyFields = []string{"reason"}

// TransferRequest is `TransferRequest` — the whole simulate surface.
type TransferRequest struct {
	CustomerID  string         `json:"customer_id"`
	Source      TransferSide   `json:"source"`
	Destination TransferSide   `json:"destination"`
	Amount      TransferAmount `json:"amount"`
	SponsorGas  *bool          `json:"sponsor_gas,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// TransferFields is the declared property set.
var TransferFields = []string{"customer_id", "source", "destination", "amount", "sponsor_gas", "metadata"}

// TransferAmount is `TransferRequest.amount`.
type TransferAmount struct {
	Side string `json:"side"`
	// Value is a decimal string, never a float: binary floating point
	// cannot represent money exactly, and a `json.Number` here would let a
	// caller round a transfer by declaring a type.
	Value string `json:"value"`
}

// TransferAmountFields is the declared property set.
var TransferAmountFields = []string{"side", "value"}

// TransferSide is `TransferSide`. Which fields are required depends on `type`,
// which selects the arm; the contract's `required` lists only `type`, and the
// arm rules are FERRY's to enforce.
type TransferSide struct {
	Type                  string `json:"type"`
	ID                    string `json:"id,omitempty"`
	Asset                 string `json:"asset,omitempty"`
	Network               string `json:"network,omitempty"`
	AccountHolder         string `json:"account_holder,omitempty"`
	BlockchainAddress     string `json:"blockchain_address,omitempty"`
	CashLocationID        string `json:"cash_location_id,omitempty"`
	CashLocationReference string `json:"cash_location_reference,omitempty"`
}

// TransferSideFields is the declared property set.
var TransferSideFields = []string{
	"type", "id", "asset", "network", "account_holder",
	"blockchain_address", "cash_location_id", "cash_location_reference",
}

// ExecuteRequest is `ExecuteRequest`. Execution takes the plan, not the
// transfer fields: the terms were pinned when the plan was issued.
type ExecuteRequest struct {
	PlanToken string `json:"plan_token"`
}

// ExecuteFields is the declared property set.
var ExecuteFields = []string{"plan_token"}

// Marshal renders a body to the bytes that will be sent.
//
// It exists so that the bytes are produced exactly once, at the call site that
// builds the request, and then carried unchanged through every retry (§5.8).
// `encoding/json` orders struct fields by declaration, so the output is
// deterministic — but the guarantee the idempotency contract needs is that the
// same bytes are *reused*, not that re-marshalling would produce the same
// ones.
func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
