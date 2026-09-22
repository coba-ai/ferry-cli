package render

import (
	"fmt"
	"io"

	"github.com/coba-ai/ferry-cli/internal/api"
)

// Corridor writes one corridor.
//
// The two `allowed_*` fields are AC40's second half and C11's whole point.
// `api.StringList` keeps `null` and `[]` apart on the way in; [list] keeps
// them apart on the way out. Converging them here would undo the decoder —
// and the two answers are opposites: `null` means the corridor enumerates
// nothing and any asset is as good as another, `[]` means it enumerates an
// empty set and none is allowed.
func Corridor(w io.Writer, c api.Corridor) {
	var f fields

	f.add("id", orNone(c.ID))
	f.add("composite", orNone(c.Composite))
	f.add("quotable", quotable(c))
	f.add("availability stated", fmt.Sprintf("%t", c.AvailabilityStated))
	f.add("amount side", orNone(c.Amount.Side))
	f.add("amount multiple", stringOr(c.Amount.Multiple, None))
	f.add("amount maximum", stringOr(c.Amount.Maximum, None))
	f.write(w, "")

	side(w, "source", c.Source)
	side(w, "destination", c.Destination)
}

func quotable(c api.Corridor) string {
	if c.Quotable {
		return "yes"
	}

	return fmt.Sprintf("no (%s)", stringOr(c.Reason, "no reason given"))
}

func side(w io.Writer, name string, s api.CorridorSide) {
	fmt.Fprintf(w, "  %s\n", name)

	var f fields

	f.add("type", orNone(s.Type))
	f.add("category", orNone(s.Category))
	f.add("allowed assets", list(s.AllowedAssets))
	f.add("allowed networks", list(s.AllowedNetworks))
	f.write(w, "    ")
}

// CorridorList writes every corridor in a list answer.
func CorridorList(w io.Writer, corridors []api.Corridor) {
	if len(corridors) == 0 {
		fmt.Fprintln(w, "no corridors")

		return
	}

	for i, c := range corridors {
		if i > 0 {
			fmt.Fprintln(w)
		}

		Corridor(w, c)
	}

	quotableCount := 0

	for _, c := range corridors {
		if c.Quotable {
			quotableCount++
		}
	}

	fmt.Fprintf(w, "\n%d corridors, %d quotable\n", len(corridors), quotableCount)
}
