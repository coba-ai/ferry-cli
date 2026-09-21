package render

import (
	"fmt"
	"io"

	"github.com/kurenn/ferry/cli/internal/api"
)

// APIKey writes one key.
//
// Never the token: the only render that prints one is [APIKeyCreated], and
// that is the whole of AC42's first half. A key read back from `GET
// /v1/api_keys/{id}` carries no `token` at all (`api_key_serializer.rb:40`),
// so there is nothing to print even if this wanted to.
func APIKey(w io.Writer, k api.APIKey) {
	var f fields

	f.add("id", orNone(k.ID))
	f.add("name", orNone(k.Name))
	f.add("environment", fmt.Sprintf("%s (%s)", orNone(k.Environment.Kind), orNone(k.Environment.ID)))
	f.add("scopes", sortedScopes(k.Scopes))
	f.add("credential", fmt.Sprintf("%s…%s", orNone(k.TokenPrefix), orNone(k.TokenLast4)))
	f.add("expires at", orNone(k.ExpiresAt))
	f.add("created at", orNone(k.CreatedAt))
	f.add("created by", orNone(k.CreatedBy))
	f.add("last used", stringOr(k.LastUsedAt, "never"))
	f.add("status", keyStatus(k))

	f.write(w, "")
}

// keyStatus reads revocation off the two nullable fields rather than off a
// `status` FERRY does not send. `revoked_at` null is an active key;
// `revoked_reason` may be null on a revoked one, which is why the two are
// separate lines rather than one sentence.
func keyStatus(k api.APIKey) string {
	if k.RevokedAt == nil {
		return "active"
	}

	return fmt.Sprintf("revoked at %s (%s)", *k.RevokedAt, stringOr(k.RevokedReason, "no reason given"))
}

// APIKeyCreated writes a freshly minted key, including its token.
//
// This is [SiteKeysCreateToken], one of AC42's two places. The token goes
// through [Reveal] rather than being interpolated directly: that is what makes
// "exactly two" a checkable claim rather than a promise, because every other
// path is then a path with no way to get a secret into a string.
//
// A key whose `201` carried no token at all is reported as such. `token` is a
// pointer for exactly this reason (`responses.go`): `""` would read as "the
// token is empty", and a caller who believed it would store nothing and think
// they had.
func APIKeyCreated(w io.Writer, k api.APIKey) {
	APIKey(w, k)

	fmt.Fprintln(w)

	if k.Token == nil {
		fmt.Fprintln(w, "This answer carried no token. FERRY returns the plaintext key once, on this")
		fmt.Fprintln(w, "response only, so there is no way to fetch it now. Revoke this key and mint another.")

		return
	}

	fmt.Fprintln(w, "token (shown once; FERRY will not return it again):")
	fmt.Fprintf(w, "  %s\n", Reveal(SiteKeysCreateToken, *k.Token))
}

// APIKeyList writes a page of keys.
func APIKeyList(w io.Writer, keys []api.APIKey) {
	if len(keys) == 0 {
		fmt.Fprintln(w, "no api keys")

		return
	}

	for i, k := range keys {
		if i > 0 {
			fmt.Fprintln(w)
		}

		APIKey(w, k)
	}
}

// Page is what a list answer said about the next one.
//
// A presentation struct, not a wire type: `internal/api` declares no `List`
// envelope (see the report accompanying this unit), so the noun package
// decodes one and hands the four facts here.
type Page struct {
	Returned int
	Limit    int
	HasMore  bool

	// NextCursor is nil when the answer carried `null`. The CLI never
	// constructs one (AC39, M40) — it is echoed back or it is absent.
	NextCursor *string

	// Followed is true when `--all` already walked the remaining pages, so
	// the render does not tell a caller to do what it just did.
	Followed bool
}

// Pagination writes what the page said about the next one.
func Pagination(w io.Writer, p Page) {
	fmt.Fprintln(w)

	var f fields

	f.add("returned", fmt.Sprintf("%d", p.Returned))
	f.add("limit", fmt.Sprintf("%d", p.Limit))
	f.add("has more", fmt.Sprintf("%t", p.HasMore))
	f.add("next cursor", stringOr(p.NextCursor, None))

	if p.HasMore && !p.Followed {
		f.add("next page", "rerun with --cursor <next cursor>, or --all to follow every page")
	}

	f.write(w, "")
}
