package transfers

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/render"
)

// bodyFlags is the `TransferRequest` surface of PLAN §5.1, one flag per
// contract field.
//
// Nothing here validates an arm, a network or an amount. The contract's
// `TransferSide` lists only `type` as required and leaves the arm rules to
// FERRY, which decides them against the customer's own instruments; a CLI
// that guessed would refuse requests FERRY accepts and would have to be
// re-released every time an arm gained a field.
type bodyFlags struct {
	customer string

	fromType                  string
	fromID                    string
	fromAsset                 string
	fromNetwork               string
	fromBlockchainAddress     string
	fromCashLocationID        string
	fromCashLocationReference string

	toType                  string
	toID                    string
	toAsset                 string
	toNetwork               string
	toAccountHolder         string
	toBlockchainAddress     string
	toCashLocationID        string
	toCashLocationReference string

	amount     string
	amountSide string

	sponsorGas bool
	metadata   []string

	// body is `--body FILE|-`: a JSON document forwarded byte for byte.
	body string
}

// fieldFlagNames is every flag [bodyFlags] reads, for the `--body` conflict
// check.
//
// It is a declared list rather than "every flag on the command" because the
// command also carries `--broadcast`, `--yes`, `--timeout` and the globals,
// and none of those describes the request body. A caller passing `--body`
// with `--yes` is doing something coherent; `--body` with `--amount` is not.
var fieldFlagNames = []string{
	"customer",
	"from-type", "from-id", "from-asset", "from-network",
	"from-blockchain-address", "from-cash-location-id", "from-cash-location-reference",
	"to-type", "to-id", "to-asset", "to-network", "to-account-holder",
	"to-blockchain-address", "to-cash-location-id", "to-cash-location-reference",
	"amount", "amount-side",
	"sponsor-gas", "metadata",
}

func bindBodyFlags(cmd *cobra.Command, f *bodyFlags) {
	fl := cmd.Flags()

	fl.StringVar(&f.customer, "customer", "", "the customer the transfer belongs to (customer_id)")

	fl.StringVar(&f.fromType, "from-type", "", "source.type, e.g. walletCrypto")
	fl.StringVar(&f.fromID, "from-id", "", "source.id")
	fl.StringVar(&f.fromAsset, "from-asset", "", "source.asset")
	fl.StringVar(&f.fromNetwork, "from-network", "", "source.network")
	fl.StringVar(&f.fromBlockchainAddress, "from-blockchain-address", "", "source.blockchain_address")
	fl.StringVar(&f.fromCashLocationID, "from-cash-location-id", "", "source.cash_location_id")
	fl.StringVar(&f.fromCashLocationReference, "from-cash-location-reference", "", "source.cash_location_reference")

	fl.StringVar(&f.toType, "to-type", "", "destination.type, e.g. bankUs")
	fl.StringVar(&f.toID, "to-id", "", "destination.id")
	fl.StringVar(&f.toAsset, "to-asset", "", "destination.asset")
	fl.StringVar(&f.toNetwork, "to-network", "", "destination.network")
	fl.StringVar(&f.toAccountHolder, "to-account-holder", "", "destination.account_holder")
	fl.StringVar(&f.toBlockchainAddress, "to-blockchain-address", "", "destination.blockchain_address")
	fl.StringVar(&f.toCashLocationID, "to-cash-location-id", "", "destination.cash_location_id")
	fl.StringVar(&f.toCashLocationReference, "to-cash-location-reference", "", "destination.cash_location_reference")

	fl.StringVar(&f.amount, "amount", "", "amount.value, as typed (never parsed as a float)")
	fl.StringVar(&f.amountSide, "amount-side", "source", "amount.side: source or destination")

	fl.BoolVar(&f.sponsorGas, "sponsor-gas", false, "ask FERRY to sponsor gas")
	fl.StringArrayVar(&f.metadata, "metadata", nil, "a k=v pair for metadata; repeatable")

	fl.StringVar(&f.body, "body", "", "a JSON TransferRequest from FILE, or - for stdin")
}

// buildBody produces the bytes that will be sent, once (AC56, AC78).
//
// The bytes are produced here and carried unchanged through `runs.Begin`,
// `api.Do` and every retry: the idempotency contract is over bytes, and a
// body re-marshalled between attempts is a different request under the same
// key. `--body` is forwarded verbatim for the same reason from the other
// direction — a caller who has a document FERRY accepts must be able to send
// exactly it, non-canonical whitespace and all (M55).
func buildBody(cmd *cobra.Command, f bodyFlags) ([]byte, error) {
	if f.body != "" {
		if conflict := changedFieldFlags(cmd); len(conflict) > 0 {
			return nil, render.Usage(
				"--body cannot be combined with %s: the document is the request, and a flag that "+
					"edited it would send bytes the caller never wrote",
				strings.Join(conflict, ", "))
		}

		return readBody(cmd, f.body)
	}

	if changed := changedFieldFlags(cmd); len(changed) == 0 {
		return nil, render.Usage(
			"a transfer needs a request: pass the body flags (--customer, --from-*, --to-*, --amount) " +
				"or `--body FILE|-`")
	}

	req := api.TransferRequest{
		CustomerID: f.customer,
		Source: api.TransferSide{
			Type:                  f.fromType,
			ID:                    f.fromID,
			Asset:                 f.fromAsset,
			Network:               f.fromNetwork,
			BlockchainAddress:     f.fromBlockchainAddress,
			CashLocationID:        f.fromCashLocationID,
			CashLocationReference: f.fromCashLocationReference,
		},
		Destination: api.TransferSide{
			Type:                  f.toType,
			ID:                    f.toID,
			Asset:                 f.toAsset,
			Network:               f.toNetwork,
			AccountHolder:         f.toAccountHolder,
			BlockchainAddress:     f.toBlockchainAddress,
			CashLocationID:        f.toCashLocationID,
			CashLocationReference: f.toCashLocationReference,
		},
		Amount: api.TransferAmount{Side: f.amountSide, Value: f.amount},
	}

	// `sponsor_gas` is a pointer in the contract's Go struct because
	// `false` and absent are different requests: absent asks FERRY for its
	// default, `false` asserts "do not". Only a caller who typed the flag
	// has asserted anything.
	if cmd.Flags().Changed("sponsor-gas") {
		v := f.sponsorGas
		req.SponsorGas = &v
	}

	metadata, err := parseMetadata(f.metadata)
	if err != nil {
		return nil, err
	}

	req.Metadata = metadata

	raw, err := api.Marshal(req)
	if err != nil {
		return nil, render.Fault(err, "the request body could not be encoded: %v", err)
	}

	return raw, nil
}

func changedFieldFlags(cmd *cobra.Command) []string {
	var out []string

	for _, name := range fieldFlagNames {
		if cmd.Flags().Changed(name) {
			out = append(out, "--"+name)
		}
	}

	sort.Strings(out)

	return out
}

// parseMetadata turns `--metadata k=v` into the object.
//
// A repeated key is a usage error rather than a silent last-wins: the two
// values are both what the caller wrote, and picking one is the CLI deciding
// which of their intentions to honour.
func parseMetadata(pairs []string) (map[string]any, error) {
	if len(pairs) == 0 {
		return nil, nil
	}

	out := make(map[string]any, len(pairs))

	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, render.Usage("--metadata %q is not k=v", pair)
		}

		if _, seen := out[key]; seen {
			return nil, render.Usage("--metadata %s was given twice", key)
		}

		out[key] = value
	}

	return out, nil
}

// readBody reads `--body FILE|-`.
//
// The bytes are checked for UTF-8 here as well as at `runs.Begin`, so the
// message names the file rather than the ledger. They are not parsed,
// reformatted or validated as JSON: FERRY is the authority on whether the
// document is a `TransferRequest`, and a CLI that rejected a body FERRY would
// accept is a second authority (C13).
func readBody(cmd *cobra.Command, source string) ([]byte, error) {
	var (
		raw []byte
		err error
	)

	if source == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(source)
	}

	if err != nil {
		// Exit 2, not exit 1. A file the caller named and that is not
		// there is a wrong command line, and `cli_fault` would tell a
		// wrapper "the CLI hit a local problem, run it again" — which for
		// a missing path is a loop. The distinction costs nothing here and
		// is the difference between a retry and a fix.
		return nil, render.Usage("the request body could not be read from %s: %v",
			describeSource(source), err)
	}

	raw = trimTrailingNewline(raw)

	if len(raw) == 0 {
		return nil, render.Usage("the request body read from %s is empty", describeSource(source))
	}

	if !utf8.Valid(raw) {
		return nil, render.Usage("the request body read from %s is not valid UTF-8", describeSource(source))
	}

	// It has to be a JSON *object*, checked here rather than left to
	// FERRY.
	//
	// Sending it would work — FERRY answers `400` — but it would spend a
	// money request's round trip to learn that a local file is malformed,
	// and it writes a run record whose body is bytes no endpoint could
	// ever accept. That record then counts as an unresolved run under C18
	// and refuses the caller's *next* transfer, which is a long way from
	// the typo that caused it.
	//
	// An array and a string are both valid JSON and neither is a request,
	// so the check is on the shape and not on parseability.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, render.Usage("the request body read from %s is not a JSON object: %v",
			describeSource(source), err)
	}

	if len(probe) == 0 {
		return nil, render.Usage("the request body read from %s is an empty JSON object",
			describeSource(source))
	}

	return raw, nil
}

// trimTrailingNewline removes the one newline a heredoc, an editor or `echo`
// adds.
//
// Exactly one, and only at the end. The body is otherwise forwarded verbatim,
// so this is the single edit this CLI makes to a caller's bytes, and it is
// made because every ordinary way of producing a file adds it — not because
// the document would be rejected with it.
func trimTrailingNewline(raw []byte) []byte {
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		return raw[:n-1]
	}

	return raw
}

func describeSource(source string) string {
	if source == "-" {
		return "stdin"
	}

	return fmt.Sprintf("%q", source)
}

// planToken reads `--plan TOKEN|-`.
func planToken(cmd *cobra.Command, value string) (string, error) {
	if value == "" {
		return "", render.Usage("--plan is required: it takes the token `ferry transfers create` printed, or -")
	}

	if value != "-" {
		return value, nil
	}

	raw, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", render.Fault(err, "the plan token could not be read from stdin: %v", err)
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", render.Usage("--plan - was given and stdin was empty")
	}

	return token, nil
}
