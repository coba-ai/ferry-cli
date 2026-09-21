package api

// The response bodies this CLI reads, hand-written and pinned
// field-for-field to `openapi.yaml` (WD1, §5.12, AC86) — AC86's five, plus
// `StoredSimulation`, the simulate arm of the `Command.result.body` union
// that A393 typed.
//
// `schema_pin_test.go` holds these structs and the document to **set
// equality**, recursively and in both directions. The direction that costs
// something is the second one: a property the contract declares and no field
// here binds is a fact FERRY sent that this CLI cannot see, and on
// `Simulation` that is a term of a price a human is about to agree to. A
// struct field the contract does not declare is the cheaper error and is red
// for the same reason.
//
// **Which fields are pointers, and why.** Absence is meaningful in these
// bodies — `Quote` and `Transaction` are allowlist projections whose contract
// says "a path the upstream body did not carry is absent here too" — so the
// rule is:
//
//   - a nested object or array is a pointer or a slice, and `nil` means the
//     body did not carry it;
//   - a scalar the contract types `[…, "null"]` is a pointer, because the
//     contract distinguishes null from a value;
//   - a scalar the contract documents as conditional is a pointer, so that
//     "the answer did not carry it" cannot be read as "it was empty";
//   - every other scalar is a plain Go value, and its zero value means the
//     upstream did not say.
//
// The third clause is AC86's second half and the one with teeth:
// `ApiKey.token`, `Simulation.plan.token`, `Simulation.meta` and
// `Simulation.meta.remediation` decode to `nil` when absent. A value type
// there would answer `""` to "did this reply carry the plan token?", and the
// caller that believed it would execute nothing and say it had.

// Principal is the `Principal` schema — `GET /v1/me`.
//
// Two shapes, one schema: an API key populates `environment` and leaves
// `user`, `role` and `environments` empty; a personal access token does the
// reverse. Branch on `Kind`, never on which field happens to be set.
type Principal struct {
	Object       string                `json:"object"`
	Kind         string                `json:"kind"`
	Organization PrincipalOrganization `json:"organization"`
	// Environment is null on a personal access token, which spans its
	// organization's environments rather than being bound to one.
	Environment *PrincipalEnvironment `json:"environment"`
	Credential  PrincipalCredential   `json:"credential"`
	// User and Role are null for an API key, which was minted by a person
	// but does not act as one.
	User *PrincipalUser `json:"user"`
	Role *string        `json:"role"`
	// Environments is empty for an API key. The contract gives its items the
	// same three properties as `environment`, so they share a type here and
	// the pin compares both paths against it.
	Environments []PrincipalEnvironment `json:"environments"`
}

// PrincipalOrganization is `Principal.organization`.
type PrincipalOrganization struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
}

// PrincipalEnvironment is `Principal.environment` and each item of
// `Principal.environments`.
type PrincipalEnvironment struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// PrincipalCredential is `Principal.credential` — the presented credential
// itself. There is no `token` here, on any response.
type PrincipalCredential struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	TokenPrefix string   `json:"token_prefix"`
	TokenLast4  string   `json:"token_last4"`
	Scopes      []string `json:"scopes"`
	ExpiresAt   string   `json:"expires_at"`
	// LastUsedAt is null until the credential's first authenticated request.
	LastUsedAt *string `json:"last_used_at"`
}

// PrincipalUser is `Principal.user`.
type PrincipalUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// APIKey is the `ApiKey` schema — the `201` of `POST /v1/api_keys`, the `200`
// of `GET /v1/api_keys/{id}` and of `POST /v1/api_keys/{id}/revoke`, and each
// item of `List.data`.
type APIKey struct {
	Object      string            `json:"object"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Environment APIKeyEnvironment `json:"environment"`
	Scopes      []string          `json:"scopes"`
	TokenPrefix string            `json:"token_prefix"`
	TokenLast4  string            `json:"token_last4"`
	ExpiresAt   string            `json:"expires_at"`
	CreatedAt   string            `json:"created_at"`
	// LastUsedAt, RevokedAt and RevokedReason are `[…, "null"]`: a key that
	// has never been used is not a key used at the zero time, and an active
	// key is not a key revoked for no reason.
	LastUsedAt    *string `json:"last_used_at"`
	RevokedAt     *string `json:"revoked_at"`
	RevokedReason *string `json:"revoked_reason"`
	CreatedBy     string  `json:"created_by"`
	// Token is the plaintext, returned once, on the `201` of
	// `POST /v1/api_keys` and on no other response — there is no endpoint
	// that will return it again. Nil means this answer did not carry it,
	// which is a different thing from a key whose token is empty.
	Token *string `json:"token"`
}

// APIKeyEnvironment is `ApiKey.environment`. Narrower than
// `Principal.environment`, which also carries `status`, so the two are
// separate types and a divergence reddens the pin rather than being absorbed.
type APIKeyEnvironment struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// Simulation is the `Simulation` schema — the `201` of
// `POST /v1/transfers/simulate`.
//
// Two properties are conditional and a body never carries both: `plan.token`
// is on the fresh `201`, `meta.remediation` on the replayed one.
// `Idempotency-Replayed: true` is how a caller tells which answer it holds
// without inspecting either field.
type Simulation struct {
	Object    string `json:"object"`
	CommandID string `json:"command_id"`
	Quote     Quote  `json:"quote"`
	Plan      Plan   `json:"plan"`
	// Meta is the replayed `201` only, and is absent from the fresh answer.
	Meta *SimulationMeta `json:"meta"`
}

// Plan is `Simulation.plan` — the priced terms `POST /v1/transfers` executes.
type Plan struct {
	Object    string `json:"object"`
	ID        string `json:"id"`
	ExpiresAt string `json:"expires_at"`
	// Token is the single-use authorization `POST /v1/transfers` takes as
	// `plan_token`. Fresh `201` only: it is never stored, so a replay answers
	// without it and will do so forever.
	Token *string `json:"token"`
}

// SimulationMeta is `Simulation.meta`.
type SimulationMeta struct {
	// Remediation is `required` inside `meta`, but `meta` itself is the
	// conditional part, so the pointer is what distinguishes "this answer
	// carried no remediation sentence" from "it carried an empty one" if a
	// `meta` ever arrives without it. §5.6 renders this sentence, and
	// rendering an empty one would tell a caller nothing while looking like
	// an answer.
	Remediation *string `json:"remediation"`
}

// StoredSimulation is the `StoredSimulation` schema — the `transfers_simulate`
// arm of `Command.result.body`, which is what a poll of a completed simulate
// command carries.
//
// It is `Simulation` minus the two conditional properties, and the omissions
// are structural rather than incidental: `plan.token` is minted with the plan
// and never stored, and `meta` is added by the replay renderer at render time,
// so a body that was written to the ledger has been through neither. A caller
// polling a completed simulate therefore gets the quote and the plan's
// identity and **never a usable plan token**.
//
// It is its own type and not `Simulation` with two nils, for the reason
// `APIKeyEnvironment` is not `PrincipalEnvironment`: a divergence between the
// stored body and the `201` should redden the pin rather than be absorbed by
// a field that was allowed to be absent anyway.
type StoredSimulation struct {
	Object    string     `json:"object"`
	CommandID string     `json:"command_id"`
	Quote     Quote      `json:"quote"`
	Plan      StoredPlan `json:"plan"`
}

// StoredPlan is `StoredSimulation.plan` — `Plan` with no `token`, because the
// ledger never stored one.
type StoredPlan struct {
	Object    string `json:"object"`
	ID        string `json:"id"`
	ExpiresAt string `json:"expires_at"`
}

// Quote is the `Quote` schema — what FERRY keeps of an upstream quote.
//
// An allowlist projection, not the upstream document: nothing here is
// required, and absence is preserved exactly, so a bank destination has no
// `cashLocationId` rather than a null one.
type Quote struct {
	CreatedAt           string         `json:"createdAt"`
	CustomerID          string         `json:"customerId"`
	Destination         *ProjectedSide `json:"destination"`
	ExpiresAt           string         `json:"expiresAt"`
	ID                  string         `json:"id"`
	Metadata            map[string]any `json:"metadata"`
	Object              string         `json:"object"`
	Pricing             *Pricing       `json:"pricing"`
	Source              *ProjectedSide `json:"source"`
	SourceToDestination string         `json:"sourceToDestination"`
	Status              string         `json:"status"`
}

// Transaction is the `Transaction` schema — the `201` of
// `POST /v1/transfers`.
//
// `201` is the upstream accepting the transfer, not settling it: read Status
// and SubStatus. A transfer can still fail after acceptance, and FERRY does
// not re-read that snapshot today.
type Transaction struct {
	CreatedAt   string         `json:"createdAt"`
	CustomerID  string         `json:"customerId"`
	Destination *ProjectedSide `json:"destination"`
	// Error is present once a transfer has failed, and absent otherwise
	// rather than null.
	Error            *TransactionError `json:"error"`
	EstimatedArrival string            `json:"estimatedArrival"`
	ExpiresAt        string            `json:"expiresAt"`
	ID               string            `json:"id"`
	Metadata         map[string]any    `json:"metadata"`
	Object           string            `json:"object"`
	Precursor        *Precursor        `json:"precursor"`
	Pricing          *Pricing          `json:"pricing"`
	Source           *ProjectedSide    `json:"source"`
	// SourceToDestination is the corridor composite the two sides resolve to.
	SourceToDestination string `json:"sourceToDestination"`
	Status              string `json:"status"`
	// SubStatus is the status-scoped sub-state, always present alongside a
	// status. AC49 renders both, and neither on its own is an outcome.
	SubStatus string `json:"subStatus"`
	UpdatedAt string `json:"updatedAt"`
}

// TransactionError is `Transaction.error`. The operator prose and the
// recovery instructions are withheld by the projection; Code is the
// machine-readable half and the one to branch on.
type TransactionError struct {
	Code        string `json:"code"`
	OccurredAt  string `json:"occurredAt"`
	Recoverable bool   `json:"recoverable"`
}

// Precursor is `Transaction.precursor` — what created this transaction. For a
// transfer executed from a plan, the quote it was priced on.
type Precursor struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// ProjectedSide is one end of a projected quote or transaction:
// `Quote.source`, `Quote.destination`, `Transaction.source` and
// `Transaction.destination` all declare the same six properties. Not
// `TransferSide`, which is the *request* shape and a different set.
type ProjectedSide struct {
	Asset          string `json:"asset"`
	CashLocationID string `json:"cashLocationId"`
	Category       string `json:"category"`
	// ID and Network are `[string, "null"]`.
	ID      *string `json:"id"`
	Network *string `json:"network"`
	Type    string  `json:"type"`
}

// Pricing is `Quote.pricing` and `Transaction.pricing`, which declare the
// same properties.
//
// Every amount is a decimal string, never a float: binary floating point
// cannot represent money exactly, and a `json.Number` here would let a
// renderer round a transfer by choosing a format.
type Pricing struct {
	Destination     *PricingSide `json:"destination"`
	EffectiveRate   string       `json:"effectiveRate"`
	ExchangeRate    string       `json:"exchangeRate"`
	FixedAmountSide string       `json:"fixedAmountSide"`
	Pair            string       `json:"pair"`
	Source          *PricingSide `json:"source"`
	SponsorGas      bool         `json:"sponsorGas"`
	SponsorGasCost  string       `json:"sponsorGasCost"`
}

// PricingSide is one side of a `pricing` block.
type PricingSide struct {
	AmountGross  string `json:"amountGross"`
	AmountNet    string `json:"amountNet"`
	Asset        string `json:"asset"`
	FeesDeducted *Fees  `json:"feesDeducted"`
}

// Fees is `pricing.<side>.feesDeducted`.
type Fees struct {
	Developer string `json:"developer"`
	Gas       string `json:"gas"`
	OMS       string `json:"oms"`
	Total     string `json:"total"`
}
