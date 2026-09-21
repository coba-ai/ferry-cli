package api

import (
	"bytes"
	"encoding/json"
)

// StringList is a JSON array that may be `null`, keeping the two apart.
//
// C11 and AC26. The contract is explicit: "`null` and `[]` are different
// answers and are written through unchanged. `null` is 'the contract
// enumerates nothing here'; `[]` is 'it enumerates an empty set'. Do not
// coerce one to the other."
//
// A plain `[]string` cannot hold that distinction — both decode to a slice
// whose only difference is nil-ness, which the first `append` or `len` in a
// renderer erases, and which `json.Marshal` then writes back as `null` for the
// empty case. A caller reading "allowed assets: (none)" for a corridor that
// enumerates nothing has been told something false about what they may send.
type StringList struct {
	// Present is false only when the value was literally `null`.
	Present bool
	Values  []string
}

var jsonNull = []byte("null")

// UnmarshalJSON keeps `null` and `[]` apart.
func (l *StringList) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), jsonNull) {
		l.Present = false
		l.Values = nil

		return nil
	}

	var values []string
	if err := json.Unmarshal(b, &values); err != nil {
		return err
	}

	l.Present = true

	if values == nil {
		values = []string{}
	}

	l.Values = values

	return nil
}

// MarshalJSON writes back what was read.
func (l StringList) MarshalJSON() ([]byte, error) {
	if !l.Present {
		return jsonNull, nil
	}

	if l.Values == nil {
		return []byte("[]"), nil
	}

	return json.Marshal(l.Values)
}

// Corridor is the `Corridor` schema.
type Corridor struct {
	Object             string         `json:"object"`
	ID                 string         `json:"id"`
	Composite          string         `json:"composite"`
	Source             CorridorSide   `json:"source"`
	Destination        CorridorSide   `json:"destination"`
	AvailabilityStated bool           `json:"availability_stated"`
	Amount             CorridorAmount `json:"amount"`
	Quotable           bool           `json:"quotable"`
	// Reason is why `quotable` is false; null when it is true.
	Reason *string `json:"reason"`
}

// CorridorFields is the declared property set.
var CorridorFields = []string{
	"object", "id", "composite", "source", "destination",
	"availability_stated", "amount", "quotable", "reason",
}

// CorridorAmount is `Corridor.amount`.
type CorridorAmount struct {
	Side string `json:"side"`
	// Multiple and Maximum are `[string, "null"]`: a corridor that states
	// no ceiling is not a corridor whose ceiling is zero.
	Multiple *string `json:"multiple"`
	Maximum  *string `json:"maximum"`
}

// CorridorAmountFields is the declared property set.
var CorridorAmountFields = []string{"side", "multiple", "maximum"}

// CorridorSide is `CorridorSide`.
type CorridorSide struct {
	Type            string     `json:"type"`
	Category        string     `json:"category"`
	AllowedAssets   StringList `json:"allowed_assets"`
	AllowedNetworks StringList `json:"allowed_networks"`
}

// CorridorSideFields is the declared property set.
var CorridorSideFields = []string{"type", "category", "allowed_assets", "allowed_networks"}
