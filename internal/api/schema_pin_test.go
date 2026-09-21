package api_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kurenn/ferry/cli/internal/api"
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
}

func (n propertyTree) names() []string {
	out := make([]string, 0, len(n.props))
	for name := range n.props {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}

// paths counts every property path in the tree, at every depth. This is what
// the floors are asserted against.
func (n propertyTree) paths() int {
	total := len(n.props)
	for _, child := range n.props {
		total += child.paths()
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

	// A composed schema is a shape this reader does not understand, and the
	// way it would fail is the dangerous one: `{allOf: [...]}` carries no
	// `properties` of its own, so it would read as a leaf and compare equal
	// to any Go scalar, with every property underneath it unbound and
	// nothing saying so. The document composes nothing today; if it starts,
	// this must be taught to resolve it rather than pass.
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "not"} {
		if _, ok := schema[keyword]; ok {
			o.t.Fatalf("openapi.yaml: %s uses %s, which this reader does not resolve; "+
				"read as-is it would be a leaf, and every property under it would be unpinned",
				where, keyword)
		}
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

// comparePropertyTrees asserts set equality at this level and then at every
// nested object, reporting each difference with the path it is at.
func comparePropertyTrees(t *testing.T, where string, got, want propertyTree, gotLabel, wantLabel string) {
	t.Helper()

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
}

func TestResponseStructsEqualTheContractSchemas(t *testing.T) {
	if len(pinnedBodies) != 5 {
		t.Fatalf("AC86 names five response bodies; this pin covers %d", len(pinnedBodies))
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
		"carry the terms of a transfer. Note that `Command.result.body` is itself `type: object`, " +
		"so the body AC74 branches on has no schema to pin — a finding for U8, not something " +
		"this file can fix.",
	"List": "the paginated envelope. Its `data` items are `ApiKey`, which is pinned above; the " +
		"envelope's own five keys belong to U4's `keys list` (AC39).",
	"Corridor": "pinned field-for-field, with `Corridor.amount` and `CorridorSide` as their own " +
		"cases, by TestRequestBodyFieldsEqualTheContract (AC25); `null` versus `[]` is AC26.",
}

func TestEveryResponseSchemaIsPinnedOrExcluded(t *testing.T) {
	o := loadContract(t)

	referenced := o.responseSchemaNames()

	// Eight is what the document has today: the four pinned above (less
	// `Quote`, which is reached through `Simulation`) plus the four excluded
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
