package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kurenn/ferry/cli/internal/outcome"
)

// ErrUsage is a request this CLI will not send: an operation it does not know,
// a path parameter it was not given, a key on a route that must not carry one
// or no key on a route that must.
//
// These are programming errors, caught before the wire rather than after —
// which is the point. An idempotent route sent without a key is a money
// request with no replay protection, and discovering that from a `400
// IDEMPOTENCY_KEY_REQUIRED` means the CLI was willing to send it.
var ErrUsage = errors.New("api: request refused before sending")

// Client sends one request and reports what came back. It decides nothing;
// see `internal/outcome`.
type Client struct {
	// BaseURL is the profile's `api_url`.
	BaseURL string

	// Token is the credential. It is never logged: every byte of debug
	// output goes through Redact.
	Token string

	// UserAgent is the whole header value. Built by UserAgent() from the
	// version package's values, which this package does not own.
	UserAgentString string

	// Environment is the environment the credential is believed to belong
	// to, for Meta.EnvironmentMismatch. Empty means no belief to check.
	Environment string

	HTTP        *http.Client
	Clock       Clock
	RetryBudget time.Duration

	// Debug, when set, receives the trace. Everything written to it passes
	// through Redact first.
	Debug io.Writer
}

// Request is one call.
type Request struct {
	Op         outcome.Operation
	PathParams map[string]string
	Query      url.Values

	// Body is sent verbatim. It is `[]byte` and not `any` on purpose: the
	// idempotency contract is over the bytes, and a client that re-marshals
	// between attempts can change them — Go's map iteration order is
	// randomised, and a re-marshalled body under the same key is
	// `IDEMPOTENCY_KEY_REUSED` at best.
	Body []byte

	// IdempotencyKey is fixed by the caller before Do is called, and Do has
	// no way to mint another. §5.8: the retry loop has no access to the
	// minter, and the structure is what enforces it.
	IdempotencyKey string
}

// Result is one answer, or the absence of one.
type Result struct {
	Op   outcome.Operation
	Meta Meta
	Body []byte

	// Envelope is set when the body decoded as the seven-key error
	// envelope. EnvelopeErr says why it did not, which AC83 turns into
	// `pending` on a money path.
	Envelope    *Envelope
	EnvelopeErr error

	// Transport is set when there was no answer at all.
	Transport *outcome.Transport

	// Attempts counts requests actually sent, including the first.
	Attempts int
}

// Input converts an answer into the decision table's tuple.
//
// This is the only place the two are joined, so there is exactly one place
// where a fact could be dropped on the way in — and the table's exhaustiveness
// tests are then tests of something a real answer can reach.
//
// parsed and transactionStatus come from the caller, because what counts as a
// readable 2xx body depends on which body was expected, and this package does
// not own the response structs (U2b).
func (r Result) Input(parsed bool, transactionStatus string) outcome.Input {
	in := outcome.Input{
		Op:        r.Op,
		Status:    r.Meta.Status,
		Transport: r.Transport,
		CommandID: r.Meta.CommandID,
		Replayed:  r.Meta.Replayed,
	}

	if r.Envelope != nil {
		in.Envelope = r.Envelope.Outcome()

		// `command_request.rb:432-439` sets the header from the detail, so
		// in practice they agree; when the header is absent the detail is
		// still the command the caller must poll.
		if in.CommandID == "" {
			in.CommandID = in.DetailCommandID()
		}
	}

	if r.Transport == nil && r.Meta.Status >= 200 && r.Meta.Status < 300 {
		in.Success = &outcome.Success{Parsed: parsed, TransactionStatus: transactionStatus}
	}

	return in
}

// UserAgent builds AC15's header value.
//
// The format lives here because this package owns what goes on the wire; the
// values do not, and are passed in — `version` is U1's, and the contract sha
// is embedded at build.
func UserAgent(version, contractSHA string) string {
	sha8 := contractSHA
	if len(sha8) > 8 {
		sha8 = sha8[:8]
	}

	return fmt.Sprintf("ferry-cli/%s (%s/%s; %s) contract/%s",
		version, runtime.GOOS, runtime.GOARCH, runtime.Version(), sha8)
}

// Do sends the request, retries what §5.8 says may be retried, and returns the
// last answer.
func (c *Client) Do(ctx context.Context, req Request) (Result, error) {
	route, ok := RouteFor(req.Op)
	if !ok {
		return Result{}, fmt.Errorf("%w: no route for operation %q", ErrUsage, req.Op)
	}

	// AC16, enforced rather than merely pinned. Both directions: a money
	// route with no key would have no replay protection, and a key on a
	// route the contract does not declare one for is a header FERRY will
	// not honour but a reader of the trace would believe in.
	switch {
	case route.Idempotent && req.IdempotencyKey == "":
		return Result{}, fmt.Errorf("%w: %s requires an Idempotency-Key", ErrUsage, req.Op)
	case !route.Idempotent && req.IdempotencyKey != "":
		return Result{}, fmt.Errorf("%w: %s does not take an Idempotency-Key", ErrUsage, req.Op)
	}

	target, err := c.url(route, req)
	if err != nil {
		return Result{}, err
	}

	class, ok := outcome.ClassOf(req.Op)
	if !ok {
		return Result{}, fmt.Errorf("%w: operation %q has no operation class", ErrUsage, req.Op)
	}

	clock := c.Clock
	if clock == nil {
		clock = realClock{}
	}

	budget := c.RetryBudget
	if budget <= 0 {
		budget = DefaultRetryBudget
	}

	started := clock.Now()

	var (
		result   Result
		attempts int
	)

	for {
		result, err = c.attempt(ctx, route, req, target)
		if err != nil {
			return Result{}, err
		}

		// Counted here rather than inside attempt, which builds a fresh
		// Result each time and would reset it.
		attempts++
		result.Attempts = attempts

		if result.Transport != nil {
			// A transport fault is not resent. Whether anything left the
			// host is exactly what AC23 cannot establish for a money
			// request, and resending under the same key is the caller's
			// decision to make with `runs resume`, not this loop's.
			return result, nil
		}

		code := ""
		if result.Envelope != nil {
			code = result.Envelope.Code
		}

		if !Retryable(class, result.Meta.Status, code, result.Meta.CommandID) {
			return result, nil
		}

		delay := time.Duration(result.Meta.RetryDelaySeconds()) * time.Second

		if clock.Now().Add(delay).Sub(started) > budget {
			// The budget is a ceiling on total wall time, checked before
			// sleeping rather than after: sleeping past it and then
			// returning would spend time to learn nothing.
			return result, nil
		}

		c.trace("retrying %s %s after %s (attempt %d)\n", route.Method, target, delay, attempts+1)

		clock.Sleep(delay)
	}
}

// attempt sends exactly one request. Every retry re-enters here with the same
// `req`, so the bytes and the key are the same by construction and not by
// discipline.
func (c *Client) attempt(ctx context.Context, route Route, req Request, target string) (Result, error) {
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, route.Method, target, body)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrUsage, err)
	}

	c.setHeaders(httpReq, req)

	watch := &writeWatch{}
	httpReq = httpReq.WithContext(httptrace.WithClientTrace(httpReq.Context(), watch.trace()))

	c.traceRequest(httpReq, req.Body)

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		c.trace("transport fault (%s): %v\n", watch.phase(), err)

		return Result{
			Op:        req.Op,
			Transport: &outcome.Transport{Phase: watch.phase(), Err: err},
		}, nil
	}

	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		// The answer's status arrived but its body did not. Bytes were on
		// the wire in both directions, so this is an after-write fault.
		c.trace("truncated response body: %v\n", err)

		return Result{
			Op:        req.Op,
			Transport: &outcome.Transport{Phase: outcome.PhaseAfterWrite, Err: err},
		}, nil
	}

	result := Result{
		Op:   req.Op,
		Meta: ReadMeta(resp.StatusCode, resp.Header, c.Environment),
		Body: raw,
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		env, err := DecodeEnvelope(raw)
		if err != nil {
			result.EnvelopeErr = err
		} else {
			result.Envelope = &env
		}
	}

	c.traceResponse(result)

	return result, nil
}

// setHeaders is AC15.
func (c *Client) setHeaders(httpReq *http.Request, req Request) {
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", c.UserAgentString)

	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	if req.IdempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}
}

func (c *Client) url(route Route, req Request) (string, error) {
	path, err := Expand(route.Path, req.PathParams)
	if err != nil {
		return "", err
	}

	base := strings.TrimSuffix(c.BaseURL, "/")

	u, err := url.Parse(base + path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUsage, err)
	}

	if len(req.Query) > 0 {
		u.RawQuery = req.Query.Encode()
	}

	return u.String(), nil
}

// Expand fills a templated path.
//
// An unfilled placeholder is an error rather than an empty segment: `GET
// /v1/api_keys/` is a different route from `GET /v1/api_keys/{id}`, and the
// CLI would be listing keys when it meant to read one.
func Expand(template string, params map[string]string) (string, error) {
	out := template

	for name, value := range params {
		placeholder := "{" + name + "}"
		if !strings.Contains(out, placeholder) {
			return "", fmt.Errorf("%w: path %q has no %s", ErrUsage, template, placeholder)
		}

		if value == "" {
			return "", fmt.Errorf("%w: path parameter %s is empty", ErrUsage, name)
		}

		out = strings.ReplaceAll(out, placeholder, url.PathEscape(value))
	}

	if i := strings.Index(out, "{"); i >= 0 {
		return "", fmt.Errorf("%w: path %q was not given %s", ErrUsage, template, out[i:])
	}

	return out, nil
}

// ResolvePollPath joins a `poll` path from a command body to the base URL.
//
// CS-13: an absolute URL in `poll` is refused. The field is a path in the
// contract ("Path to poll for this command"), and honouring an absolute URL
// there would let a response body redirect the CLI — carrying the
// `Authorization` header — to a host the profile never named.
func (c *Client) ResolvePollPath(poll string) (string, error) {
	if poll == "" {
		return "", fmt.Errorf("%w: empty poll path", ErrUsage)
	}

	if strings.Contains(poll, "://") || strings.HasPrefix(poll, "//") {
		return "", fmt.Errorf("%w: poll path %q is absolute; only a path may be followed", ErrUsage, poll)
	}

	if !strings.HasPrefix(poll, "/") {
		poll = "/" + poll
	}

	return strings.TrimSuffix(c.BaseURL, "/") + poll, nil
}

func (c *Client) trace(format string, args ...any) {
	if c.Debug == nil {
		return
	}

	fmt.Fprint(c.Debug, Redact(fmt.Sprintf(format, args...)))
}

func (c *Client) traceRequest(httpReq *http.Request, body []byte) {
	if c.Debug == nil {
		return
	}

	var b strings.Builder

	fmt.Fprintf(&b, "> %s %s\n", httpReq.Method, httpReq.URL)

	names := make([]string, 0, len(httpReq.Header))
	for name := range httpReq.Header {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		fmt.Fprintf(&b, "> %s: %s\n", name, RedactedHeaderValue(name, httpReq.Header.Get(name)))
	}

	if body != nil {
		fmt.Fprintf(&b, "> %s\n", body)
	}

	// The whole block goes through Redact again, not only the header values:
	// the header pass is a targeted rule and this is the backstop, and a
	// backstop that is skipped for one of the two paths is not one.
	fmt.Fprint(c.Debug, Redact(b.String()))
}

func (c *Client) traceResponse(r Result) {
	if c.Debug == nil {
		return
	}

	var b strings.Builder

	fmt.Fprintf(&b, "< %d\n", r.Meta.Status)
	fmt.Fprintf(&b, "< %s\n", r.Body)

	fmt.Fprint(c.Debug, Redact(b.String()))
}
