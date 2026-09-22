package api_test

import (
	"encoding/json"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/api"
)

// AC26 and C11. `null` and `[]` are different answers.
//
// The contract says so in as many words: "`null` is 'the contract enumerates
// nothing here'; `[]` is 'it enumerates an empty set'. Do not coerce one to the
// other." A caller deciding what asset to send reads the first as "this side
// is unconstrained, ask FERRY" and the second as "nothing may be sent here",
// and a CLI that renders both as "(none)" has answered a question it was not
// asked.

func TestStringListKeepsNullAndEmptyApart(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		present bool
		values  []string
	}{
		{"null", `null`, false, nil},
		{"an empty array", `[]`, true, []string{}},
		{"a populated array", `["usdc","usdt"]`, true, []string{"usdc", "usdt"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got api.StringList

			if err := json.Unmarshal([]byte(c.json), &got); err != nil {
				t.Fatalf("decoding %s: %v", c.json, err)
			}

			if got.Present != c.present {
				t.Errorf("Present: got %v, want %v", got.Present, c.present)
			}

			if len(got.Values) != len(c.values) {
				t.Fatalf("Values: got %v, want %v", got.Values, c.values)
			}

			for i := range c.values {
				if got.Values[i] != c.values[i] {
					t.Errorf("Values[%d]: got %q, want %q", i, got.Values[i], c.values[i])
				}
			}

			// And back out again as it came in. A decoder that keeps the
			// distinction and an encoder that loses it would still print
			// the wrong thing in `--output json`.
			out, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("re-encoding: %v", err)
			}

			if string(out) != c.json {
				t.Errorf("round trip: %s became %s", c.json, out)
			}
		})
	}
}

// The distinction survives a whole corridor body, which is where it actually
// has to hold: nested two levels down, through the struct the renderer reads.
func TestCorridorDecodingPreservesNullVersusEmpty(t *testing.T) {
	body := `{
		"object": "corridor",
		"id": "walletCrypto_bankUs",
		"composite": "walletCrypto:usdc:polygon->bankUs:usd:ach",
		"source": {
			"type": "walletCrypto",
			"category": "crypto",
			"allowed_assets": ["usdc"],
			"allowed_networks": null
		},
		"destination": {
			"type": "bankUs",
			"category": "fiat",
			"allowed_assets": [],
			"allowed_networks": ["ach"]
		},
		"availability_stated": false,
		"amount": { "side": "either", "multiple": null, "maximum": "10000.00" },
		"quotable": true,
		"reason": null
	}`

	var got api.Corridor

	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding a corridor: %v", err)
	}

	if !got.Source.AllowedAssets.Present || len(got.Source.AllowedAssets.Values) != 1 {
		t.Errorf("source.allowed_assets: got %+v", got.Source.AllowedAssets)
	}

	// "The contract enumerates nothing here."
	if got.Source.AllowedNetworks.Present {
		t.Error("source.allowed_networks was null and decoded as an enumerated set")
	}

	// "It enumerates an empty set."
	if !got.Destination.AllowedAssets.Present {
		t.Error("destination.allowed_assets was [] and decoded as null")
	}

	if len(got.Destination.AllowedAssets.Values) != 0 {
		t.Errorf("destination.allowed_assets: got %v, want an empty set", got.Destination.AllowedAssets.Values)
	}

	// The two nullable strings on `amount`, for the same reason: a corridor
	// that states no ceiling is not one whose ceiling is zero.
	if got.Amount.Multiple != nil {
		t.Errorf("amount.multiple: got %q, want nil", *got.Amount.Multiple)
	}

	if got.Amount.Maximum == nil || *got.Amount.Maximum != "10000.00" {
		t.Errorf("amount.maximum: got %v", got.Amount.Maximum)
	}

	if got.Reason != nil {
		t.Errorf("reason: got %q, want nil", *got.Reason)
	}
}

// An absent key is not an enumerated set either. The contract declares both
// fields on every side, so this should not happen — but decoding it as "[]"
// would be the same lie as decoding `null` that way.
func TestAbsentListIsNotAnEmptySet(t *testing.T) {
	var got api.CorridorSide

	if err := json.Unmarshal([]byte(`{"type":"cash","category":"cash"}`), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if got.AllowedAssets.Present {
		t.Error("an absent allowed_assets decoded as an enumerated set")
	}
}

func TestStringListRejectsANonArray(t *testing.T) {
	var got api.StringList

	if err := json.Unmarshal([]byte(`"usdc"`), &got); err == nil {
		t.Error("a bare string decoded as a list")
	}
}
