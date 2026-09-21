package api

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrResultBodyShape is returned when `Command.result.body` is JSON but is
// not one of the two shapes the contract declares.
//
// A distinct error and not a zero value, for the reason `ErrEnvelopeShape` is:
// AC74 decides exit 0 from exit 8 on `result.body.status`, and a body this
// decoder does not recognise must not reach that decision as "a transaction
// with an empty status". "Something answered and it was not a shape this
// contract describes" is a fact worth carrying, on a path that may have moved
// money.
var ErrResultBodyShape = errors.New("api: command result body is not a shape the contract declares")

// ResultBodyArms is `Command.result.body.discriminator.mapping`, in the
// document's order: the value of `object` and the schema it selects.
//
// The list is here rather than only in the switch below so that the pin can
// hold it to the document in both directions. A third arm added to the
// contract and not to this decoder would otherwise be an `ErrResultBodyShape`
// discovered at the wire.
var ResultBodyArms = []string{"transaction", "simulation"}

// CommandResultBody is `Command.result.body` — the one union in this
// contract, and the only place where the shape of an answer is chosen by the
// body rather than by the route.
//
// **The two arms are what two different commands stored.** A
// `transfers_execute` command stored the projected `Transaction` it was
// answered with; a `transfers_simulate` command stored the simulate `201`,
// which is a `StoredSimulation` and carries no `status`. That difference is
// AC74's: `status` is how a completed execute says whether money moved, and
// its absence on a completed simulate is normal rather than missing.
//
// Read `Object`, which is the contract's discriminator, and then the arm it
// names. Do not branch on which pointer happens to be non-nil — that is the
// same mistake as branching on which `details` keys are present, and it makes
// an unrecognised body indistinguishable from a simulate.
type CommandResultBody struct {
	// Object is `result.body.object`, the discriminator. It is always one of
	// ResultBodyArms once this has decoded without error.
	Object string
	// Transaction is the `transfers_execute` arm, non-nil exactly when
	// Object is "transaction".
	Transaction *Transaction
	// Simulation is the `transfers_simulate` arm, non-nil exactly when
	// Object is "simulation".
	Simulation *StoredSimulation
}

// UnmarshalJSON reads the discriminator first and then the arm it names.
//
// It refuses an `object` it does not recognise rather than decoding into
// whichever arm happens to fit. The arms overlap — both carry `object`, both
// carry an id-shaped string — so "try each in turn and keep the one that
// parses" would silently pick one, and `encoding/json` ignores unknown keys,
// which means a `StoredSimulation` decodes into a `Transaction` without
// complaint and answers "" to a question about `status`.
func (b *CommandResultBody) UnmarshalJSON(data []byte) error {
	var probe struct {
		Object *string `json:"object"`
	}

	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("%w: %v", ErrResultBodyShape, err)
	}

	if probe.Object == nil {
		return fmt.Errorf("%w: no `object` to discriminate on", ErrResultBodyShape)
	}

	*b = CommandResultBody{Object: *probe.Object}

	switch *probe.Object {
	case "transaction":
		b.Transaction = &Transaction{}

		return b.arm(data, b.Transaction)
	case "simulation":
		b.Simulation = &StoredSimulation{}

		return b.arm(data, b.Simulation)
	default:
		*b = CommandResultBody{}

		return fmt.Errorf("%w: object=%q is not one of %v", ErrResultBodyShape, *probe.Object, ResultBodyArms)
	}
}

// arm decodes into the selected arm, and leaves nothing half-populated behind
// if that fails.
func (b *CommandResultBody) arm(data []byte, into any) error {
	if err := json.Unmarshal(data, into); err != nil {
		*b = CommandResultBody{}

		return fmt.Errorf("%w: %v", ErrResultBodyShape, err)
	}

	return nil
}
