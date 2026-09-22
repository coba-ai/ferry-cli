package api_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/outcome"
)

// AC86: the Go structs for `Principal`, `ApiKey`, `Simulation`, `Quote` and
// `Transaction` have field sets **equal** to the corresponding `openapi.yaml`
// schemas' properties, both directions, recursively.
//
// Three things about how this is built, each of which is the difference
// between a pin and a decoration.
//
// **The expectation comes from the document, parsed here, and from nothing
// else.** Not from a list kept alongside the structs, and not from the
// structs themselves: a detector that draws its inputs from the thing it
// checks cannot fail. `contract_test.go`'s request-body pin can afford a
// declared list because it holds that list to *both* the document and the
// tags; there is no such list here, so there is nothing to keep in step and
// nothing to accidentally agree with itself.
//
// **Both directions, at every level.** A subset check — "every field I
// declare is a property" — catches a typo and nothing else. The failure that
// costs something is a property the contract declares that no field binds:
// the CLI decodes the body, drops that key on the floor, and renders a price
// with a term missing. `assertSetsEqual` (contract_test.go) reports both
// differences, and the walk applies it at every nested object.
//
// **A floor under every comparison.** Two empty sets are equal. A reader that
// stopped recognising `properties`, a `$ref` that stopped resolving, a walk
// that mistook an object for a leaf — each would make this file green while
// asserting nothing, so each schema declares the number of property paths it
// must have found before the comparison counts for anything.

// maxSchemaDepth bounds both walks. The deepest real path is
// `Transaction.pricing.source.feesDeducted.total` at four, so this is slack
// rather than a limit — its job is to turn a `$ref` cycle into a failure
// instead of a hang.
const maxSchemaDepth = 12

// propertyTree is the shape both sides are reduced to before they are
// compared: the names a schema (or a struct) declares at this level, and for
// each name that is itself an object, its own tree. A leaf has no properties.
type propertyTree struct {
	props map[string]propertyTree
	// arms is non-nil for a `oneOf` node, and is then the whole of the node:
	// the discriminator's value and the tree of the schema that value
	// selects. A union has no properties of its own, so a tree has `props`
	// or `arms` and never both — the arms are alternatives, and merging them
	// into one property set would describe a body no answer ever is.
	arms map[string]propertyTree
	// discriminator is the property the arms are chosen by (`object`, here).
	discriminator string
}

func (n propertyTree) names() []string {
	out := make([]string, 0, len(n.props))
	for name := range n.props {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}

func (n propertyTree) armNames() []string {
	out := make([]string, 0, len(n.arms))
	for name := range n.arms {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}

// paths counts every property path in the tree, at every depth. This is what
// the floors are asserted against. A union contributes its arms' paths and
// nothing of its own, because it has nothing of its own.
func (n propertyTree) paths() int {
	total := len(n.props)
	for _, child := range n.props {
		total += child.paths()
	}

	for _, arm := range n.arms {
		total += arm.paths()
	}

	return total
}

// resolveRef follows a `$ref` to the schema it names.
//
// A `$ref` this function cannot follow is fatal, never an empty schema: a
// dangling reference that silently yielded no properties would make the
// comparison below vacuous in the worst possible direction — the struct would
// be compared against nothing and pass with every field unbound.
func (o openapi) resolveRef(where string, schema map[string]any, depth int) map[string]any {
	o.t.Helper()

	ref, ok := schema["$ref"].(string)
	if !ok {
		return schema
	}

	if depth > maxSchemaDepth {
		o.t.Fatalf("openapi.yaml: %s: $ref chain is deeper than %d; it is probably a cycle", where, maxSchemaDepth)
	}

	if !strings.HasPrefix(ref, "#/") {
		o.t.Fatalf("openapi.yaml: %s: $ref %q is not a local reference; this reader resolves nothing else", where, ref)
	}

	cur := any(o.doc)

	for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s: $ref %q passes through %q, which is not a mapping", where, ref, segment)
		}

		cur, ok = m[segment]
		if !ok {
			o.t.Fatalf("openapi.yaml: %s: $ref %q does not resolve — %q is not there", where, ref, segment)
		}
	}

	target, ok := cur.(map[string]any)
	if !ok {
		o.t.Fatalf("openapi.yaml: %s: $ref %q resolves to something that is not a schema", where, ref)
	}

	return o.resolveRef(where, target, depth+1)
}

// schemaTree reduces a schema to its property tree.
func (o openapi) schemaTree(where string, schema map[string]any, depth int) propertyTree {
	o.t.Helper()

	if depth > maxSchemaDepth {
		o.t.Fatalf("openapi.yaml: %s is nested deeper than %d levels; this reader has lost its footing", where, maxSchemaDepth)
	}

	schema = o.resolveRef(where, schema, 0)

	// A composed schema carries no `properties` of its own, so read as-is it
	// would be a leaf and would compare equal to any Go scalar, with every
	// property underneath it unbound and nothing saying so. That is the
	// dangerous failure, and it is why these are fatal rather than skipped.
	//
	// `oneOf` is the one this reader now resolves — A393 expressed
	// `Command.result.body` as a discriminated union — and it resolves it
	// into arms rather than into a merged property set, because a union's
	// arms are alternatives and a merge would document a body that no
	// answer is.
	for _, keyword := range []string{"allOf", "anyOf", "not"} {
		if _, ok := schema[keyword]; ok {
			o.t.Fatalf("openapi.yaml: %s uses %s, which this reader does not resolve; "+
				"read as-is it would be a leaf, and every property under it would be unpinned",
				where, keyword)
		}
	}

	if _, ok := schema["oneOf"]; ok {
		return o.unionTree(where, schema, depth)
	}

	raw, ok := schema["properties"]
	if !ok {
		// An array's properties are its items': the Go side binds it as a
		// slice, and the element type is what carries the fields.
		if items, ok := schema["items"]; ok {
			m, ok := items.(map[string]any)
			if !ok {
				o.t.Fatalf("openapi.yaml: %s.items is not a mapping", where)
			}

			return o.schemaTree(where+"[]", m, depth+1)
		}

		// A scalar, or an object with `additionalProperties: true` and no
		// declared keys (`metadata`). Both are leaves: there is nothing named
		// to bind.
		return propertyTree{}
	}

	props, ok := raw.(map[string]any)
	if !ok {
		o.t.Fatalf("openapi.yaml: %s.properties is not a mapping", where)
	}

	tree := propertyTree{props: map[string]propertyTree{}}

	for name, v := range props {
		m, ok := v.(map[string]any)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s.properties.%s is not a mapping", where, name)
		}

		tree.props[name] = o.schemaTree(where+"."+name, m, depth+1)
	}

	return tree
}

// unionTree reduces a `oneOf` node to its arms, keyed by the discriminator
// value that selects each one.
//
// Everything this insists on is something a decoder needs and the `oneOf`
// list alone does not give it.
//
// **A discriminator.** Without one, the only way to decode is to try each arm
// and keep whichever parses. The arms here overlap — both carry `object`,
// both carry id-shaped strings — and `encoding/json` ignores unknown keys, so
// that guess does not fail, it succeeds wrongly.
//
// **The mapping and the arm list held equal, both ways.** An arm the mapping
// omits is one no discriminating decoder can reach; a mapping entry with no
// arm names a schema the union does not contain. Either is a contract that
// reads as complete and is not.
//
// **Each arm's own discriminator value.** The mapping says `simulation`
// selects `StoredSimulation`; `StoredSimulation.object` says `const:
// simulation`. Those are two independent statements in the document and they
// must agree, or a body that satisfies the arm is one the mapping sends
// somewhere else.
func (o openapi) unionTree(where string, schema map[string]any, depth int) propertyTree {
	o.t.Helper()

	list, ok := schema["oneOf"].([]any)
	if !ok || len(list) == 0 {
		o.t.Fatalf("openapi.yaml: %s.oneOf is not a non-empty list", where)
	}

	discriminator, ok := schema["discriminator"].(map[string]any)
	if !ok {
		o.t.Fatalf("openapi.yaml: %s is a oneOf with no discriminator; the arms overlap, so a "+
			"decoder that tried each in turn would not fail, it would succeed wrongly", where)
	}

	property, ok := discriminator["propertyName"].(string)
	if !ok || property == "" {
		o.t.Fatalf("openapi.yaml: %s.discriminator has no propertyName", where)
	}

	mapping, ok := discriminator["mapping"].(map[string]any)
	if !ok || len(mapping) == 0 {
		o.t.Fatalf("openapi.yaml: %s.discriminator has no mapping; without one the discriminator "+
			"names a property and not what its values mean", where)
	}

	// The arms, by the schema name each `$ref` ends in.
	armRefs := make([]string, 0, len(list))

	for i, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s.oneOf[%d] is not a mapping", where, i)
		}

		ref, ok := m["$ref"].(string)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s.oneOf[%d] is not a $ref; this reader maps arms to "+
				"discriminator values by reference, and an inline arm has no name to map", where, i)
		}

		armRefs = append(armRefs, ref)
	}

	mappedRefs := make([]string, 0, len(mapping))
	for _, raw := range mapping {
		ref, ok := raw.(string)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s.discriminator.mapping has a non-string target", where)
		}

		mappedRefs = append(mappedRefs, ref)
	}

	assertSetsEqual(o.t, where+".discriminator.mapping", mappedRefs, armRefs,
		"the discriminator mapping's targets", "the oneOf arms")

	if depth > maxSchemaDepth {
		o.t.Fatalf("openapi.yaml: %s nests deeper than %d", where, maxSchemaDepth)
	}

	tree := propertyTree{arms: map[string]propertyTree{}, discriminator: property}

	for value, raw := range mapping {
		ref, _ := raw.(string)
		armWhere := where + "(" + value + ")"
		arm := o.resolveRef(armWhere, map[string]any{"$ref": ref}, 0)

		// The arm's own statement of which value it answers to.
		props, ok := arm["properties"].(map[string]any)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s resolves to a schema with no properties", armWhere)
		}

		field, ok := props[property].(map[string]any)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s does not declare the discriminator property %q, so a "+
				"body of this arm carries nothing to discriminate on", armWhere, property)
		}

		if got, ok := field["const"]; !ok || got != value {
			o.t.Fatalf("openapi.yaml: the mapping sends %s=%q to %s, whose own %s is %v; a body "+
				"satisfying that arm would be routed somewhere else", property, value, ref, property, got)
		}

		tree.arms[value] = o.schemaTree(armWhere, arm, depth+1)
	}

	return tree
}

// required reads a schema's `required` list, following a `$ref` first.
func (o openapi) required(where string, schema map[string]any) []string {
	o.t.Helper()

	schema = o.resolveRef(where, schema, 0)

	raw, ok := schema["required"]
	if !ok {
		return nil
	}

	list, ok := raw.([]any)
	if !ok {
		o.t.Fatalf("openapi.yaml: %s.required is not a list", where)
	}

	out := make([]string, 0, len(list))

	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s.required has a non-string entry", where)
		}

		out = append(out, s)
	}

	return out
}

// goTree reduces a Go type to its property tree, by **struct tag**.
//
// The names come from the tags and not from a Go-name-to-snake-case function,
// because such a function is a second implementation of the wire format that
// could agree with itself while disagreeing with `encoding/json`. What binds
// `PlanToken` to `plan_token` at runtime is the tag, so the tag is what is
// compared.
func goTree(t *testing.T, where string, rt reflect.Type, depth int) propertyTree {
	t.Helper()

	if depth > maxSchemaDepth {
		t.Fatalf("%s is nested deeper than %d levels", where, maxSchemaDepth)
	}

	// A type with its own codec is a leaf, because its Go fields are not
	// what it puts on the wire. `api.StringList` is the one here: it decodes
	// a `[array, "null"]` into `{Present, Values}`, two names the contract
	// has never heard of and which carry no json tags, so walking its fields
	// would compare private bookkeeping against a schema and fail on both
	// directions at once.
	//
	// This is a leaf and not a skip. If such a type ever marshalled to an
	// *object*, the schema node opposite it would still declare properties,
	// and comparing them against this empty set is how that gets found —
	// the equality below reports what the document has and the Go side does
	// not, which is exactly the right complaint.
	if isCustomCodec(rt) {
		return propertyTree{}
	}

	for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
		rt = rt.Elem()
	}

	if rt.Kind() != reflect.Struct {
		return propertyTree{}
	}

	tree := propertyTree{props: map[string]propertyTree{}}

	for i := range rt.NumField() {
		f := rt.Field(i)

		if f.Anonymous {
			t.Fatalf("%s.%s is embedded; `encoding/json` flattens it, so the field set this walk "+
				"reports would not be the one on the wire", where, f.Name)
		}

		if f.PkgPath != "" {
			t.Fatalf("%s.%s is unexported, so it decodes nothing", where, f.Name)
		}

		tag, ok := f.Tag.Lookup("json")
		if !ok {
			t.Fatalf("%s.%s has no json tag, so what it binds on the wire is its Go name", where, f.Name)
		}

		name, _, _ := strings.Cut(tag, ",")

		switch name {
		case "":
			t.Fatalf("%s.%s has a json tag with no name (%q)", where, f.Name, tag)
		case "-":
			// Skipping it would be the one way a field could sit in a
			// response struct without the contract declaring it.
			t.Fatalf("%s.%s is tagged json:\"-\"; a response struct holds what the contract "+
				"declares and nothing else", where, f.Name)
		}

		tree.props[name] = goTree(t, where+"."+name, f.Type, depth+1)
	}

	return tree
}

// isCustomCodec reports whether the type, or a pointer to it, decides its own
// JSON representation. Both interfaces are checked: `MarshalJSON` is what a
// re-encoded body is written through and `UnmarshalJSON` is what a response
// is read through, and a type declaring either has taken the wire shape out
// of its field list.
func isCustomCodec(rt reflect.Type) bool {
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	unmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

	for _, iface := range []reflect.Type{marshaler, unmarshaler} {
		if rt.Implements(iface) || reflect.PointerTo(rt).Implements(iface) {
			return true
		}
	}

	return false
}

// comparePropertyTrees asserts set equality at this level and then at every
// nested object, reporting each difference with the path it is at.
func comparePropertyTrees(t *testing.T, where string, got, want propertyTree, gotLabel, wantLabel string) {
	t.Helper()

	// A union node has no properties of its own, so without this it would
	// reduce to the empty set — and so does a Go scalar. The two would
	// compare equal, and every property under both arms would be unpinned
	// with nothing saying so. That is the same failure the `allOf` fatal
	// exists to prevent, one level further in.
	if got.arms != nil || want.arms != nil {
		assertSetsEqual(t, where+" (union arms)", got.armNames(), want.armNames(), gotLabel, wantLabel)

		if got.discriminator != want.discriminator {
			t.Errorf("%s: %s discriminates on %q, %s on %q",
				where, gotLabel, got.discriminator, wantLabel, want.discriminator)
		}

		for value, wantArm := range want.arms {
			gotArm, ok := got.arms[value]
			if !ok {
				continue // Already reported by the set comparison above.
			}

			comparePropertyTrees(t, where+"("+value+")", gotArm, wantArm, gotLabel, wantLabel)
		}

		return
	}

	assertSetsEqual(t, where, got.names(), want.names(), gotLabel, wantLabel)

	for name, wantChild := range want.props {
		gotChild, ok := got.props[name]
		if !ok {
			continue // Already reported by the set comparison above.
		}

		comparePropertyTrees(t, where+"."+name, gotChild, wantChild, gotLabel, wantLabel)
	}
}

// pinnedBodies is AC86's five, and the floor each comparison must clear.
//
// The floors are the property-path counts the document has today, rounded
// down with room to spare, so that ordinary contract growth does not touch
// them and a walk that collapsed does. They are not a second expectation:
// nothing here says *which* paths, only that the reader found a tree of
// roughly the right size before the equality was asserted. Without them, a
// `$ref` that stopped resolving or a `properties` key that moved would leave
// two empty sets, which are equal.
var pinnedBodies = []struct {
	schema   string
	value    any
	minPaths int
}{
	{schema: "Principal", value: api.Principal{}, minPaths: 20},
	{schema: "ApiKey", value: api.APIKey{}, minPaths: 12},
	{schema: "Simulation", value: api.Simulation{}, minPaths: 45},
	{schema: "Quote", value: api.Quote{}, minPaths: 40},
	{schema: "Transaction", value: api.Transaction{}, minPaths: 45},
	// A393's sixth: the simulate arm of the `Command.result.body` union.
	// Reached through `Command` rather than answered directly, which is why
	// it is not one of AC86's five.
	{schema: "StoredSimulation", value: api.StoredSimulation{}, minPaths: 45},
	// A402's seventh: the paginated envelope, which until now was declared
	// in `internal/noun/keys` and pinned by nothing. The instantiation is
	// the point — `List[ApiKey]` is what `GET /v1/api_keys` answers, and it
	// is a concrete struct, so the walk goes through `data` into the item
	// type and pins the key alongside the wrapper. Its floor is the
	// envelope's five keys plus `ApiKey`'s sixteen.
	{schema: "List", value: api.List[api.APIKey]{}, minPaths: 18},
}

func TestResponseStructsEqualTheContractSchemas(t *testing.T) {
	if len(pinnedBodies) != 7 {
		t.Fatalf("AC86 names five response bodies, A393 adds StoredSimulation and A402 adds List; "+
			"this pin covers %d", len(pinnedBodies))
	}

	for _, c := range pinnedBodies {
		t.Run(c.schema, func(t *testing.T) {
			// The reader is built per subtest so that a fatal — a dangling
			// `$ref`, a shape it does not recognise — stops the subtest that
			// found it, rather than calling FailNow on the parent from
			// another goroutine.
			o := loadContract(t)

			schema := o.mapAt("components", "schemas", c.schema)
			want := o.schemaTree(c.schema, schema, 0)

			if n := want.paths(); n < c.minPaths {
				t.Fatalf("read only %d property paths out of the %s schema, expected at least %d; "+
					"the reader has stopped matching and every comparison below would be vacuous",
					n, c.schema, c.minPaths)
			}

			got := goTree(t, reflect.TypeOf(c.value).Name(), reflect.TypeOf(c.value), 0)

			comparePropertyTrees(t, c.schema, got, want,
				"the Go struct's json tags", "openapi.yaml")
		})
	}
}

// corridorEnvelopePath is where `GET /v1/corridors` declares its answer. It
// is inline rather than a `components.schemas` entry, which is why this
// envelope needs a test of its own and cannot be a row in `pinnedBodies`.
var corridorEnvelopePath = []string{
	"paths", "/v1/corridors", "get", "responses", "200", "content", "application/json", "schema",
}

// A402: the second envelope, `{object, data}` with no cursor fields.
//
// A341 read the two local `List` declarations as two copies of one shape.
// They are not, and the distinction is the whole reason this is a separate
// assertion: if `api.Collection` and `api.List` were the same type, this
// test would fail with three properties the contract does not declare —
// `has_more`, `next_cursor` and `limit` — invented for an endpoint that
// pages nothing. That failure is the guard against the tidier-looking fix.
//
// The walk goes through `data` into `Corridor`, so this pins the item type
// recursively as well: `Corridor`, `CorridorSide` on both sides and the
// inline `amount` object. `allowed_assets` and `allowed_networks` are
// `api.StringList`, which has its own codec and is therefore a leaf on the
// Go side; the contract makes them arrays of strings, which is a leaf on the
// document side too, so the two agree for the same reason rather than by
// accident.
func TestTheCorridorEnvelopeEqualsTheContract(t *testing.T) {
	o := loadContract(t)

	want := o.schemaTree("GET /v1/corridors.200", o.mapAt(corridorEnvelopePath...), 0)

	// The floor. Two envelope keys plus twenty paths of `Corridor`; a reader
	// that resolved the envelope but lost the `$ref` under `data.items`
	// would land at two, and two-against-two would be green.
	if n := want.paths(); n < 18 {
		t.Fatalf("read only %d property paths out of the GET /v1/corridors 200 schema, expected "+
			"at least 18; the reader has stopped matching and the comparison below is vacuous", n)
	}

	rt := reflect.TypeOf(api.Collection[api.Corridor]{})

	comparePropertyTrees(t, "GET /v1/corridors.200", goTree(t, "Collection[Corridor]", rt, 0), want,
		"the Go struct's json tags", "openapi.yaml")
}

// And the two envelopes are not interchangeable, stated as an assertion
// rather than left to the two pins above to imply.
//
// `api.List` and `api.Collection` agree on `object` and `data` and differ on
// exactly the three pagination keys. A future tidy-up that gave `Collection`
// a `has_more`, or took `limit` off `List` to make one serve both, would be
// caught by the pins — but this says in one place what the difference is and
// that it is deliberate, which is the thing A341 did not know.
func TestTheTwoEnvelopesDifferByThePaginationKeysAndNothingElse(t *testing.T) {
	paginated := goTree(t, "List", reflect.TypeOf(api.List[api.APIKey]{}), 0)
	whole := goTree(t, "Collection", reflect.TypeOf(api.Collection[api.Corridor]{}), 0)

	if len(paginated.props) == 0 || len(whole.props) == 0 {
		t.Fatal("one of the envelopes read as having no properties; the walk is broken and the " +
			"difference below would be vacuous")
	}

	var extra []string

	for name := range paginated.props {
		if _, ok := whole.props[name]; !ok {
			extra = append(extra, name)
		}
	}

	assertSetsEqual(t, "what the paginated envelope adds", extra,
		[]string{"has_more", "next_cursor", "limit"},
		"api.List minus api.Collection", "the three pagination keys")

	for name := range whole.props {
		if _, ok := paginated.props[name]; !ok {
			t.Errorf("api.Collection declares %q, which api.List does not; the unpaginated "+
				"envelope is the paginated one without its cursor fields, not a third shape", name)
		}
	}
}

// resultBodyArmTypes binds each discriminator value to the Go type that
// decodes it. Held to `CommandResultBody`'s own fields below, both ways, so
// it cannot name an arm the decoder does not have or miss one it does.
var resultBodyArmTypes = map[string]any{
	"transaction": api.Transaction{},
	"simulation":  api.StoredSimulation{},
}

// commandResultBodyPath is where the union lives in the document.
var commandResultBodyPath = []string{
	"components", "schemas", "Command", "properties", "result", "properties", "body",
}

// A393: `Command.result.body` is a union, and this pins it as one.
//
// The reason it is worth a test of its own rather than a row in
// `pinnedBodies` is that a union is the shape a property-set comparison is
// blindest to. `{oneOf: [...]}` carries no `properties`, so before A393
// taught the reader to resolve it, the node would have reduced to the empty
// set — and so does any Go scalar. The comparison would have been between
// nothing and nothing, and every field of both arms would have been unpinned.
//
// What is asserted, and what each part would catch:
//
//   - the document's arms equal `api.ResultBodyArms`, both ways. That list is
//     what the decoder switches on, so an arm added to the contract and not
//     to the switch is a body answered with `ErrResultBodyShape` at the wire;
//     an entry in the list the contract does not declare is a branch that can
//     never be taken.
//   - the arms equal `resultBodyArmTypes`'s keys, and that map's values equal
//     the union struct's pointer fields, both ways. Together these say the
//     Go type has exactly one arm per discriminator value.
//   - each arm's fields equal its schema's properties, recursively, which is
//     `comparePropertyTrees` doing what it does for the other six.
func TestCommandResultBodyIsTheUnionTheContractDeclares(t *testing.T) {
	o := loadContract(t)

	want := o.schemaTree("Command.result.body", o.mapAt(commandResultBodyPath...), 0)

	if want.arms == nil {
		t.Fatal("Command.result.body did not read as a union; before A393 it was a bare " +
			"`type: object`, and a reader that has gone back to seeing one would compare " +
			"every field below against nothing")
	}

	// The floor. Both arms together are the whole of two money bodies, so a
	// reader that resolved the union but lost an arm's `$ref` would land far
	// under this.
	if n := want.paths(); n < 100 {
		t.Fatalf("read only %d property paths out of the Command.result.body union, expected at "+
			"least 100; the reader has stopped matching and the comparisons below are vacuous", n)
	}

	assertSetsEqual(t, "Command.result.body arms", api.ResultBodyArms, want.armNames(),
		"api.ResultBodyArms, which the decoder switches on", "openapi.yaml's discriminator mapping")

	if want.discriminator != "object" {
		t.Errorf("the contract discriminates result.body on %q; the decoder reads `object`",
			want.discriminator)
	}

	// The registry, against the decoder's own fields. A pointer field is an
	// arm; `Object` is the discriminator and is not.
	declared := make([]string, 0, len(resultBodyArmTypes))
	for value, v := range resultBodyArmTypes {
		declared = append(declared, reflect.TypeOf(v).Name())

		if _, ok := want.arms[value]; !ok {
			t.Errorf("resultBodyArmTypes binds %q, which the contract's mapping does not name", value)
		}
	}

	bound := []string{}
	union := reflect.TypeOf(api.CommandResultBody{})

	for i := range union.NumField() {
		if f := union.Field(i); f.Type.Kind() == reflect.Pointer {
			bound = append(bound, f.Type.Elem().Name())
		}
	}

	assertSetsEqual(t, "CommandResultBody's arms", bound, declared,
		"the pointer fields of api.CommandResultBody", "resultBodyArmTypes")

	for value, wantArm := range want.arms {
		v, ok := resultBodyArmTypes[value]
		if !ok {
			t.Errorf("the contract's mapping names %q, which no Go type decodes", value)

			continue
		}

		rt := reflect.TypeOf(v)

		comparePropertyTrees(t, "Command.result.body("+value+")",
			goTree(t, rt.Name(), rt, 0), wantArm,
			"the Go struct's json tags", "openapi.yaml")
	}
}

// And that the decoder routes by the discriminator rather than by what
// happens to parse.
//
// These bodies are two- and three-key probes of `encoding/json`'s behaviour
// against these types and do not claim to be FERRY responses (C13). The claim
// under test is the routing, and the case that matters is the third: a
// simulate body has no key a transaction body lacks the ability to ignore, so
// "decode into Transaction and see" succeeds on it.
func TestCommandResultBodyRoutesOnTheDiscriminator(t *testing.T) {
	var executed api.CommandResultBody

	decode(t, `{"object":"transaction","status":"COMPLETED","subStatus":"SETTLED"}`, &executed)

	if executed.Transaction == nil {
		t.Fatal("an object=transaction body decoded to no transaction arm")
	}

	if executed.Simulation != nil {
		t.Errorf("an object=transaction body also populated the simulation arm: %+v", executed.Simulation)
	}

	if executed.Transaction.Status != "COMPLETED" {
		t.Errorf("status: got %q", executed.Transaction.Status)
	}

	var simulated api.CommandResultBody

	decode(t, `{"object":"simulation","command_id":"cmd_1","plan":{"id":"plan_1"}}`, &simulated)

	if simulated.Simulation == nil {
		t.Fatal("an object=simulation body decoded to no simulation arm")
	}

	if simulated.Transaction != nil {
		t.Errorf("an object=simulation body also populated the transaction arm: %+v", simulated.Transaction)
	}

	if simulated.Simulation.CommandID != "cmd_1" {
		t.Errorf("command_id: got %q", simulated.Simulation.CommandID)
	}

	// The two refusals. An unrecognised `object` must not be decoded into
	// whichever arm tolerates it, and a body with no `object` has nothing to
	// route on — AC74 reads `result.body.status`, and both of these would
	// otherwise reach it as a transaction with an empty status.
	for _, body := range []string{
		`{"object":"receipt","status":"COMPLETED"}`,
		`{"status":"COMPLETED"}`,
	} {
		var got api.CommandResultBody

		err := json.Unmarshal([]byte(body), &got)
		if !errors.Is(err, api.ErrResultBodyShape) {
			t.Errorf("decoding %s: got %v, want ErrResultBodyShape", body, err)
		}

		if got.Transaction != nil || got.Simulation != nil || got.Object != "" {
			t.Errorf("decoding %s left %+v behind; a refused body must leave nothing a caller "+
				"could read as an arm", body, got)
		}
	}
}

// A393: the three `error.details` keys this CLI branches on are declared by
// the contract, and the envelope still decodes the ones it does not.
//
// `ErrorDetails` is an **open** object — `additionalProperties: true` — and
// that is deliberate: which keys a body carries follows from `code`, several
// codes emit more than one shape under one code, and a closed set would make
// adding a diagnostic key a breaking change for every generated client. The
// cost of open is that a client cannot be sure a key it reads is one FERRY
// sends, which is exactly the risk here: C19 splits `IDEMPOTENCY_KEY_REUSED`
// on `details.reason`, and reading a key the renderer does not emit gets ""
// and takes the safe-looking branch on a double-send.
//
// So the three names are taken from the **accessors**, by feeding each one a
// details map carrying only its key and seeing whether it comes back. That is
// what makes this not a restatement of a list: a rename in `internal/outcome`
// leaves the accessor reading a key the document does not declare, and this
// goes red.
//
// This direction is CLI-into-document and is the only one a Go test can take.
// The other direction — every key FERRY renders appears in `ErrorDetails` —
// is Ruby's, and `spec/docs/api_docs_spec.rb` sweeps the emitters for it.
func TestTheDetailKeysThisCLIBranchesOnAreDeclared(t *testing.T) {
	read := map[string]func(outcome.Input) string{
		"reason":     outcome.Input.Reason,
		"state":      outcome.Input.State,
		"command_id": outcome.Input.DetailCommandID,
	}

	o := loadContract(t)
	declared := o.mapAt("components", "schemas", "ErrorDetails", "properties")

	if len(declared) < 20 {
		t.Fatalf("ErrorDetails declares only %d properties; the reader has stopped matching, and "+
			"the lookups below would be against an empty vocabulary", len(declared))
	}

	for key, accessor := range read {
		t.Run(key, func(t *testing.T) {
			probe := outcome.Input{Envelope: &outcome.Envelope{
				Code:    "IDEMPOTENCY_KEY_REUSED",
				Details: map[string]any{key: "sentinel"},
			}}

			if got := accessor(probe); got != "sentinel" {
				t.Fatalf("the accessor did not read details[%q] (got %q); this case names a key "+
					"nothing reads, so the assertion below proves nothing", key, got)
			}

			property, ok := declared[key].(map[string]any)
			if !ok {
				t.Fatalf("this CLI branches on error.details.%s and openapi.yaml's ErrorDetails "+
					"does not declare it. Under `additionalProperties: true` nothing else will "+
					"say so, and the branch not taken is the one that matters (C19)", key)
			}

			// A declared key with no prose is one a caller cannot tell the
			// meaning of, and these three mean different things per code.
			if s, _ := property["description"].(string); len(s) < 20 {
				t.Errorf("ErrorDetails.%s has no usable description (%q); which codes emit it is "+
					"the whole of what a caller needs to branch safely", key, s)
			}
		})
	}

	// Open, and the envelope decoder must therefore keep what it is not
	// expecting. A struct here would drop every key but the three above,
	// silently, which is the failure `additionalProperties: true` invites.
	if open, _ := o.mapAt("components", "schemas", "ErrorDetails")["additionalProperties"].(bool); !open {
		t.Error("ErrorDetails is no longer an open object; if the contract has closed the set, " +
			"this test's reasoning about unknown keys needs re-reading")
	}

	carried, err := api.DecodeEnvelope([]byte(`{"error":{"code":"X","message":"m","retriable":false,` +
		`"retry_after_seconds":null,"details":{"reason":"different_request","not_yet_documented":1},` +
		`"request_id":null,"docs_url":"d"}}`))
	if err != nil {
		t.Fatalf("decoding an envelope with an undeclared detail key: %v", err)
	}

	if _, ok := carried.Details["not_yet_documented"]; !ok {
		t.Errorf("the decoder dropped a detail key the contract does not name: %v", carried.Details)
	}
}

// The conditional half of AC86: `ApiKey.token`, `Simulation.plan.token`,
// `Simulation.meta` and `Simulation.meta.remediation` are pointers, so an
// answer that does not carry them decodes to nil.
//
// Each case names the property as the wire spells it and finds the field by
// its **json tag** on the type that declares it, never by Go field name: a
// retagged field must fail to be found rather than quietly leave the case
// asserting a pointer somewhere else.
var conditionalProperties = []struct {
	path string
	// parent is the schema path whose `required` list must not name the last
	// segment — the document's own statement that the property is
	// conditional. Empty where the property is required *within* a
	// conditional parent, which `why` then explains.
	parent []string
	value  any
	why    string
}{
	{
		path:   "token",
		parent: []string{"components", "schemas", "ApiKey"},
		value:  api.APIKey{},
		why:    "the plaintext is returned once, on the 201 of POST /v1/api_keys, and on no other response",
	},
	{
		path:   "token",
		parent: []string{"components", "schemas", "Simulation", "properties", "plan"},
		value:  api.Plan{},
		why:    "the plan token is minted with the plan and never stored, so a replay answers without it",
	},
	{
		path:   "meta",
		parent: []string{"components", "schemas", "Simulation"},
		value:  api.Simulation{},
		why:    "meta is the replayed 201 only",
	},
	{
		path:  "remediation",
		value: api.SimulationMeta{},
		why: "remediation is required within meta, but meta is conditional; nil keeps " +
			"\"this answer carried no remediation\" apart from an empty sentence",
	},
}

func TestConditionalPropertiesArePointers(t *testing.T) {
	if len(conditionalProperties) != 4 {
		t.Fatalf("AC86 and A311/V2 name four conditional properties; this covers %d", len(conditionalProperties))
	}

	for _, c := range conditionalProperties {
		name := reflect.TypeOf(c.value).Name() + "." + c.path

		t.Run(name, func(t *testing.T) {
			o := loadContract(t)

			f := fieldByJSONName(t, reflect.TypeOf(c.value), c.path)

			if f.Type.Kind() != reflect.Pointer {
				t.Errorf("%s is %s, not a pointer: an answer that does not carry the property "+
					"decodes to that type's zero value, which no caller can tell from a present "+
					"and empty one — and %s (AC86)", name, f.Type, c.why)
			}

			if c.parent == nil {
				return
			}

			// The document's half of the claim. If the contract ever makes
			// this property required, the pointer is still correct Go but
			// the reason above has stopped being true, and that is worth
			// discovering here rather than in a renderer.
			for _, r := range o.required(strings.Join(c.parent, "."), o.mapAt(c.parent...)) {
				if r == c.path {
					t.Errorf("openapi.yaml now lists %q in %s.required, so it is no longer conditional; "+
						"this pin's reason for the pointer (%s) needs re-reading",
						c.path, strings.Join(c.parent, "."), c.why)
				}
			}
		})
	}
}

// And that the pointers do what pointers are for here.
//
// The bodies below are not FERRY responses and do not claim to be — C13
// forbids a Go test asserting against a hand-written response body. They are
// two-key probes of `encoding/json`'s behaviour against these structs: the
// property is there, or it is not, and the question is only what the field
// holds afterwards. Both directions, because "nil when absent" is half a
// claim; a field that was always nil would satisfy it.
func TestConditionalPropertiesDecodeAsNilWhenAbsent(t *testing.T) {
	var absentKey, presentKey api.APIKey

	decode(t, `{"object":"api_key"}`, &absentKey)
	decode(t, `{"object":"api_key","token":"ferry_sk_sandbox_x"}`, &presentKey)

	if absentKey.Token != nil {
		t.Errorf("ApiKey.token absent decoded to %q, not nil", *absentKey.Token)
	}

	if presentKey.Token == nil || *presentKey.Token != "ferry_sk_sandbox_x" {
		t.Errorf("ApiKey.token present decoded to %v, not the value in the body", presentKey.Token)
	}

	var fresh, replayed api.Simulation

	decode(t, `{"object":"simulation","plan":{"token":"ferry_plan_x"}}`, &fresh)
	decode(t, `{"object":"simulation","plan":{},"meta":{"remediation":"simulate again"}}`, &replayed)

	if fresh.Plan.Token == nil || *fresh.Plan.Token != "ferry_plan_x" {
		t.Errorf("plan.token present decoded to %v, not the value in the body", fresh.Plan.Token)
	}

	if fresh.Meta != nil {
		t.Errorf("meta absent decoded to %+v, not nil", *fresh.Meta)
	}

	if replayed.Plan.Token != nil {
		t.Errorf("plan.token absent decoded to %q, not nil", *replayed.Plan.Token)
	}

	if replayed.Meta == nil {
		t.Fatal("meta present decoded to nil")
	}

	if replayed.Meta.Remediation == nil || *replayed.Meta.Remediation != "simulate again" {
		t.Errorf("meta.remediation present decoded to %v, not the value in the body", replayed.Meta.Remediation)
	}

	var noRemediation api.SimulationMeta

	decode(t, `{}`, &noRemediation)

	if noRemediation.Remediation != nil {
		t.Errorf("meta.remediation absent decoded to %q, not nil", *noRemediation.Remediation)
	}
}

// Which schemas this file is answerable for, in both directions.
//
// AC86 names five bodies. That list is only as good as the claim that five is
// all there are, so the schemas every JSON response references are read out of
// the document and each must be either pinned above or excluded here with a
// reason. A sixth body would otherwise arrive unpinned and unnoticed — the
// CLI would decode it into nothing, and nothing would say so.
var unpinnedBodySchemas = map[string]string{
	"Error": "the seven-key envelope. Decoded by envelope.go, which refuses a body missing any " +
		"of the seven, and pinned against Error.error.required by AC17 (contract_test.go).",
	"Command": "the asynchronous result. internal/outcome decides on it and its `state` enum is " +
		"pinned against the document by AC25 (contract_test.go); AC86 names the five bodies that " +
		"carry the terms of a transfer. Its `result.body` is no longer the bare `type: object` " +
		"U2b found (A321): A393 typed it as a discriminated union, and " +
		"TestCommandResultBodyIsTheUnionTheContractDeclares pins both arms field-for-field. " +
		"The envelope around them — `state`, `last_error`, `contradiction` — has no Go struct " +
		"here yet because nothing in internal/api decodes a command body; when the poll loop " +
		"lands, that struct belongs in this list rather than in this exclusion.",
	"Corridor": "pinned field-for-field, with `Corridor.amount` and `CorridorSide` as their own " +
		"cases, by TestRequestBodyFieldsEqualTheContract (AC25); `null` versus `[]` is AC26. " +
		"A402 pins it a second time and recursively, as the item type of the envelope " +
		"TestTheCorridorEnvelopeEqualsTheContract reads.",
}

func TestEveryResponseSchemaIsPinnedOrExcluded(t *testing.T) {
	o := loadContract(t)

	referenced := o.responseSchemaNames()

	// Eight is what the document has today: the five pinned above that a
	// response references directly (`Quote` is reached through `Simulation`
	// and `StoredSimulation` through `Command`) plus the three excluded
	// below. It is a floor on the reader, not a count of the contract — but a
	// change that leaves a response body unreferenced should be read here
	// rather than quietly shrinking what this test is answerable for.
	if len(referenced) < 8 {
		t.Fatalf("found only %d schemas referenced by a JSON response (%v); the reader has "+
			"stopped matching", len(referenced), referenced)
	}

	pinned := map[string]bool{}
	for _, c := range pinnedBodies {
		pinned[c.schema] = true
	}

	for _, name := range referenced {
		if pinned[name] || unpinnedBodySchemas[name] != "" {
			continue
		}

		t.Errorf("openapi.yaml answers a request with the %s schema, which AC86 does not pin and "+
			"this file does not exclude. A body nothing pins is a body this CLI can decode into "+
			"nothing without anything saying so.", name)
	}

	// The other direction: an exclusion for a schema no response uses is a
	// reason that has gone stale, and a stale reason reads as a decision.
	inDocument := map[string]bool{}
	for _, name := range referenced {
		inDocument[name] = true
	}

	for name := range unpinnedBodySchemas {
		if !inDocument[name] {
			t.Errorf("this file excludes the %s schema from AC86, but no JSON response references "+
				"it any more", name)
		}
	}

	for _, c := range pinnedBodies {
		if _, ok := o.mapAt("components", "schemas")[c.schema]; !ok {
			t.Errorf("AC86 pins the %s schema, which openapi.yaml no longer declares", c.schema)
		}
	}
}

// responseSchemaNames collects every `components.schemas` name reachable from
// a response's `application/json` schema — directly, through a `$ref`, or as
// the items of an inline array (`GET /v1/corridors` answers that shape).
func (o openapi) responseSchemaNames() []string {
	o.t.Helper()

	found := map[string]bool{}

	var collect func(where string, schema map[string]any, depth int)

	collect = func(where string, schema map[string]any, depth int) {
		if depth > maxSchemaDepth {
			o.t.Fatalf("openapi.yaml: %s nests deeper than %d", where, maxSchemaDepth)
		}

		if ref, ok := schema["$ref"].(string); ok {
			if name, ok := strings.CutPrefix(ref, "#/components/schemas/"); ok {
				found[name] = true

				return
			}

			// A `$ref` into `components.responses`, which carries its own
			// content block. Follow it rather than stopping: those are where
			// the shared `Unauthorized`, `NotFound` and friends live.
			collect(where, o.resolveRef(where, schema, 0), depth+1)

			return
		}

		if content, ok := schema["content"].(map[string]any); ok {
			if body, ok := content["application/json"].(map[string]any); ok {
				if inner, ok := body["schema"].(map[string]any); ok {
					collect(where+".schema", inner, depth+1)
				}
			}

			return
		}

		// A composed response body. No response is one today — A393's union
		// is nested inside `Command`, and this walk stops at a schema
		// boundary by design — but a union answered directly would
		// otherwise contribute no names at all, and its arms would be
		// bodies nothing here is answerable for.
		for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
			if arms, ok := schema[keyword].([]any); ok {
				for i, raw := range arms {
					if m, ok := raw.(map[string]any); ok {
						collect(fmt.Sprintf("%s.%s[%d]", where, keyword, i), m, depth+1)
					}
				}
			}
		}

		if props, ok := schema["properties"].(map[string]any); ok {
			for name, v := range props {
				if m, ok := v.(map[string]any); ok {
					collect(where+"."+name, m, depth+1)
				}
			}
		}

		if items, ok := schema["items"].(map[string]any); ok {
			collect(where+"[]", items, depth+1)
		}
	}

	for path, raw := range o.mapAt("paths") {
		item, ok := raw.(map[string]any)
		if !ok {
			o.t.Fatalf("openapi.yaml: paths[%q] is not a mapping", path)
		}

		for method, rawOp := range item {
			op, ok := rawOp.(map[string]any)
			if !ok {
				continue
			}

			responses, ok := op["responses"].(map[string]any)
			if !ok {
				continue
			}

			for status, rawResp := range responses {
				resp, ok := rawResp.(map[string]any)
				if !ok {
					continue
				}

				collect(path+"."+method+"."+status, resp, 0)
			}
		}
	}

	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}

// fieldByJSONName finds the struct field whose json tag names the property,
// and fails if there is none.
func fieldByJSONName(t *testing.T, rt reflect.Type, property string) reflect.StructField {
	t.Helper()

	for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
		rt = rt.Elem()
	}

	if rt.Kind() != reflect.Struct {
		t.Fatalf("%s is not a struct", rt)
	}

	for i := range rt.NumField() {
		f := rt.Field(i)

		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == property {
			return f
		}
	}

	t.Fatalf("no field of %s binds the property %q; this case names a property the struct does "+
		"not have, so it asserts nothing", rt, property)

	return reflect.StructField{}
}

func decode(t *testing.T, body string, into any) {
	t.Helper()

	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
}
