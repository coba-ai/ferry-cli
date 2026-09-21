package api

// The list envelopes, and there are two of them.
//
// A341 recorded this as one wire shape declared twice — `keys` and
// `corridors` each holding a local `List` — and half of that is right: both
// were declared outside `internal/api` and neither was pinned. The shape is
// not one shape. `openapi.yaml`'s `List` says so in its own description:
// `GET /v1/api_keys` paginates and answers five keys; `GET /v1/corridors`
// answers `{object, data}` and no cursor fields, "because it is a fixed
// table rather than a growing collection". Collapsing the two would give the
// corridor catalogue three fields no answer carries — the same defect A341
// names, pointing the other way, and with the added cost that `keys list`
// re-encodes its envelope, so a fabricated `has_more: false` would be a
// fabrication this CLI prints. So: one type per declared shape, each pinned
// to the schema that declares it (A402).
//
// **Why the item type is a type parameter.** These envelopes carry different
// items — `ApiKey` under `List`, `Corridor` under `Collection` — and the
// three ways to write that in Go are not equally good here:
//
//   - `[]json.RawMessage`, with each caller decoding its own items. This is
//     the one that looks safest and is worst, because it puts the items
//     beyond the reach of the pin this file exists to add:
//     `schema_pin_test.go` walks the Go type's fields against the schema's
//     properties, and a `json.RawMessage` has no fields to walk. The
//     envelope would be pinned and `List.data.items` — the `ApiKey` a caller
//     actually reads — would compare against nothing and pass. It also costs
//     a second decode pass over bytes already decoded once.
//   - `[]any`, decoding to `map[string]any`. Same blindness at the pin, plus
//     every reader becomes a type assertion, and a wrong one is a runtime
//     panic where a type parameter is a compile error. It is not the
//     least-bad option, so it is not used.
//   - an `interface{ ... }` the item types implement. There is no behaviour
//     to name: `ApiKey` and `Corridor` share no method a renderer wants, so
//     the interface would exist only to be type-asserted back out of, which
//     is `[]any` with extra ceremony.
//
// A type parameter keeps `Data` a `[]ApiKey` at every use site, decodes in
// one pass, and — the reason that settles it — instantiates to a concrete
// struct that `reflect` can walk, so the pin reaches the items.

// List is the `List` schema — the paginated envelope of `GET /v1/api_keys`.
//
// Field order is the order the contract declares and the order `keys list`
// prints: the aggregated page is re-encoded from this struct (AC39), so
// reordering these fields reorders keys in the JSON a caller parses.
type List[T any] struct {
	Object string `json:"object"`
	Data   []T    `json:"data"`

	// HasMore and NextCursor are the two halves of the same fact and both
	// are read: a cursor is followed only when `has_more` says there is
	// another page, and it is never constructed (AC39).
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
	Limit      int     `json:"limit"`
}

// Collection is the unpaginated envelope of `GET /v1/corridors`.
//
// Declared inline in the document rather than under `components.schemas`, so
// its pin is addressed by path. Two keys, and the absence of the other three
// is the contract's statement that the corridor catalogue is answered whole:
// there is no cursor to follow and nothing for a caller to page.
type Collection[T any] struct {
	Object string `json:"object"`
	Data   []T    `json:"data"`
}
