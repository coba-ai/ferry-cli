package transfers

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/fault"
	"github.com/kurenn/ferry-cli/internal/outcome"
	"github.com/kurenn/ferry-cli/internal/render"
	"github.com/kurenn/ferry-cli/internal/runs"
	"github.com/kurenn/ferry-cli/internal/version"
)

// report writes the one document, or the one text block, and returns the
// error carrying the exit code (AC41, §5.9).
//
// It is the only writer of stdout on the money path. Every refusal above it
// returns a `render.Error` instead, and the root turns that into the same
// document — so the "exactly one document" property is a property of there
// being exactly one place that writes one.
func (r *runner) report(a *answer) error {
	// AC69(c): after `Record` returned nil and before a byte of the report
	// is written. It models a bug in this CLI's own rendering, which is why
	// it is a plain panic and not a [fault.Crash] — the process stays up and
	// has to answer for itself, and C17 says the answer is 6.
	fault.Panic(fault.RenderPanicsAfter201)

	out := a.out
	out.Warnings = append(append([]string{}, out.Warnings...), a.warnings...)

	if r.local.Mode == render.ModeJSON {
		if err := render.WriteDocument(r.cmd.OutOrStdout(), r.document(a, out)); err != nil {
			return err
		}
	} else {
		r.text(r.cmd.OutOrStdout(), a, out)
	}

	if out.Exit == 0 {
		return nil
	}

	return render.FromOutcome(out, "%s", describe(a))
}

func (r *runner) document(a *answer, out outcome.Outcome) render.Document {
	doc := render.Document{
		FerryCLI: render.CLIBlock{
			Version:     version.Get().Version,
			RunID:       render.Optional(r.runID()),
			Profile:     r.local.Profile(),
			Environment: render.Optional(r.environment()),
		},
		Outcome:  render.NewOutcomeBlock(out),
		Response: a.body,
	}

	if a.last.Transport == nil && a.last.Meta.Status != 0 {
		doc.HTTP = render.NewHTTPBlock(a.last)
	}

	if a.last.Envelope != nil {
		doc.Error = errorBlock(a.last.Body)
	}

	return doc
}

// errorBlock lifts the `error` object out of an envelope body verbatim.
//
// Verbatim because `details` carries `reason`, `command_id` and `state` —
// the fields a caller acts on — and re-encoding through a Go type would drop
// whatever this CLI's structs do not name (A321).
func errorBlock(body []byte) json.RawMessage {
	var outer struct {
		Error json.RawMessage `json:"error"`
	}

	if err := json.Unmarshal(body, &outer); err != nil {
		return nil
	}

	return outer.Error
}

// describe is the sentence the exit code is attached to.
func describe(a *answer) string {
	res := a.last

	switch {
	case res.Transport != nil:
		return fmt.Sprintf("no answer arrived (%s): %v", res.Transport.Phase, res.Transport.Err)

	case a.poll != nil && a.poll.Command != nil:
		cmd := a.poll.Command

		return fmt.Sprintf("command %s is %s", cmd.ID, cmd.State)

	case res.Envelope != nil:
		return fmt.Sprintf("FERRY answered %d %s: %s", res.Meta.Status, res.Envelope.Code, res.Envelope.Message)

	case res.EnvelopeErr != nil:
		return fmt.Sprintf("FERRY answered %d with a body that is not the error envelope: %v",
			res.Meta.Status, res.EnvelopeErr)

	case res.Meta.Status == 0:
		return "nothing was sent"

	default:
		return fmt.Sprintf("FERRY answered %d", res.Meta.Status)
	}
}

// text is PLAN §5.9's money block: run, outcome, status, transaction or
// command, and what to do next.
//
// The vocabulary is AC49's. "success" and "✓" do not appear, and cannot be
// added without failing `render_test.go`: an execute that FERRY accepted is
// `accepted_upstream`, which means the money is somewhere between two ledgers
// and will be reconciled later. A CLI that said "success" there would be
// making a claim about settlement that nothing it can see supports.
func (r *runner) text(w io.Writer, a *answer, out outcome.Outcome) {
	var b strings.Builder

	field := func(key, value string) {
		if value == "" {
			return
		}

		fmt.Fprintf(&b, "%-14s %s\n", key+":", value)
	}

	field("run", r.runID())
	field("profile", r.local.Profile())
	field("environment", r.environment())
	field("outcome", fmt.Sprintf("%s (exit %d)", out.Class, out.Exit))
	field("money", string(out.Money))

	if a.sim != nil {
		r.quote(&b, a.sim)
	}

	if a.txn != nil {
		field("transaction", a.txn.ID)
		field("status", a.txn.Status)
		field("sub-status", a.txn.SubStatus)
		field("arrival", a.txn.EstimatedArrival)
		b.WriteString("This transfer was accepted upstream. It is not settled; reconcile it later.\n")
	}

	if a.poll != nil && a.poll.Command != nil {
		field("command", a.poll.Command.ID)
		field("command state", a.poll.Command.State)
	} else if id := commandID(a.res); id != "" {
		field("command", id)
	}

	for _, warning := range out.Warnings {
		fmt.Fprintf(&b, "warning:       %s\n", warning)
	}

	if out.Next != "" {
		fmt.Fprintf(&b, "next:          %s\n", out.Next)
	}

	io.WriteString(w, r.prefixed(b.String()))
}

// quote renders what the caller is paying, and prints the plan token exactly
// where the API handed one out (AC46, C6).
func (r *runner) quote(b *strings.Builder, sim *api.Simulation) {
	fmt.Fprintf(b, "%-14s %s\n", "quote:", sim.Quote.ID)

	if p := sim.Quote.Pricing; p != nil {
		if p.Source != nil {
			fmt.Fprintf(b, "%-14s %s %s\n", "from:", p.Source.AmountGross, p.Source.Asset)
		}

		if p.Destination != nil {
			fmt.Fprintf(b, "%-14s %s %s\n", "to:", p.Destination.AmountNet, p.Destination.Asset)
		}

		fmt.Fprintf(b, "%-14s %s\n", "rate:", p.EffectiveRate)

		if p.Source != nil && p.Source.FeesDeducted != nil {
			fmt.Fprintf(b, "%-14s %s\n", "fees:", p.Source.FeesDeducted.Total)
		}
	}

	fmt.Fprintf(b, "%-14s %s\n", "corridor:", sim.Quote.SourceToDestination)
	fmt.Fprintf(b, "%-14s %s\n", "plan:", sim.Plan.ID)
	fmt.Fprintf(b, "%-14s %s\n", "plan expires:", expiry(sim.Plan.ExpiresAt, r.deps.now()))

	if sim.Plan.Token != nil && *sim.Plan.Token != "" {
		// The one sink a plan token may be written through (AC42). It is
		// printed exactly when the API handed one out, and the reveal is
		// what `render`'s secret sweep counts.
		fmt.Fprintf(b, "%-14s %s\n", "plan token:", render.Reveal(render.SiteSimulatePlanToken, *sim.Plan.Token))
	}

	if sim.Meta != nil && sim.Meta.Remediation != nil && *sim.Meta.Remediation != "" {
		fmt.Fprintf(b, "%-14s %s\n", "note:", *sim.Meta.Remediation)
	}
}

// displayPlan is what a human is asked to agree to (AC48, AC93).
//
// It is written to stderr, so `--output json`'s single document is still the
// only thing on stdout while a human is answering a question (C10). For
// `transfers execute --plan` there is no quote to show — the token was
// priced by another invocation — so it shows what it holds and says so,
// rather than implying it checked terms it never saw.
func (r *runner) displayPlan(w io.Writer, step *runs.Step) {
	var b strings.Builder

	if r.sim != nil {
		r.quoteForPrompt(&b, r.sim)
	} else {
		fmt.Fprintf(&b, "%-14s %s\n", "plan token:", tokenPrefix(step))
		b.WriteString("This invocation holds a plan token and not the quote that priced it.\n" +
			"`ferry transfers create` printed the amounts when the plan was issued.\n")
	}

	fmt.Fprintf(&b, "%-14s %s\n", "profile:", r.local.Profile())
	fmt.Fprintf(&b, "%-14s %s\n", "environment:", r.environmentForPrompt())
	fmt.Fprintf(&b, "%-14s %s\n", "run:", r.runID())

	io.WriteString(w, r.prefixed(b.String()))
}

// quoteForPrompt is the quote without the token.
//
// The token is not shown at the prompt: it is a bearer authorisation for
// this transfer, the human is about to decide whether to spend it, and a
// declined plan whose token scrolled past on a shared terminal has leaked
// the one secret the decline existed to protect.
func (r *runner) quoteForPrompt(b *strings.Builder, sim *api.Simulation) {
	if p := sim.Quote.Pricing; p != nil {
		if p.Source != nil {
			fmt.Fprintf(b, "%-14s %s %s\n", "from:", p.Source.AmountGross, p.Source.Asset)
		}

		if p.Destination != nil {
			fmt.Fprintf(b, "%-14s %s %s\n", "to:", p.Destination.AmountNet, p.Destination.Asset)
		}

		fmt.Fprintf(b, "%-14s %s\n", "rate:", p.EffectiveRate)

		if p.Source != nil && p.Source.FeesDeducted != nil {
			fmt.Fprintf(b, "%-14s %s\n", "fees:", p.Source.FeesDeducted.Total)
		}
	}

	fmt.Fprintf(b, "%-14s %s\n", "corridor:", sim.Quote.SourceToDestination)
	fmt.Fprintf(b, "%-14s %s\n", "plan expires:", expiry(sim.Plan.ExpiresAt, r.deps.now()))
}

// environmentForPrompt spells a live environment in capitals.
func (r *runner) environmentForPrompt() string {
	env := r.environment()
	if env == "" {
		return render.None
	}

	if r.live {
		return strings.ToUpper(env)
	}

	return env
}

// prefixed puts `LIVE` in front of every line for a live credential (AC58).
//
// Every line, not the first: a caller reading the middle of a scrollback, or
// grepping one line out of a log, must not have to have seen the top of the
// block to know which environment's money this is.
func (r *runner) prefixed(text string) string {
	if !r.live || text == "" {
		return text
	}

	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "LIVE " + line
	}

	return strings.Join(lines, "\n") + "\n"
}

// expiry renders an instant and how long is left, because "expires at
// 14:02:11Z" is not something a human can act on without knowing the time.
func expiry(raw string, now time.Time) string {
	at, ok := parseTime(raw)
	if !ok {
		if raw == "" {
			return render.None
		}

		return raw
	}

	remaining := at.Sub(now).Round(time.Second)
	if remaining <= 0 {
		return fmt.Sprintf("%s (expired)", raw)
	}

	return fmt.Sprintf("%s (%s left)", raw, remaining)
}

// tokenPrefix is as much of a plan token as a prompt may show.
func tokenPrefix(step *runs.Step) string {
	if step == nil || !step.HasPlanToken() {
		return render.None
	}

	token := *step.PlanToken
	if len(token) <= 16 {
		return token
	}

	return token[:16] + "…"
}
