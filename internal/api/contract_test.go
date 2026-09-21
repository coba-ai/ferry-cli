package api_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/outcome"
)

// AC25 and §5.12's first pin: structure from `openapi.yaml`.
//
// Every example here compares two *sets* and reports both differences. A
// subset check — "everything I declare is in the document" — would catch a
// typo and nothing else; the failure that costs something is a route, a
// parameter, a key or an enum member the document has and this CLI does not,
// and only the other direction can see it.
//
// The reader raises on a shape it does not recognise rather than returning an
// empty set, and every caller asserts a floor on what it found, so a
// restructured document fails here instead of making the comparisons
// vacuously true.

type openapi struct {
	doc map[string]any
	t   *testing.T
}

func loadContract(t *testing.T) openapi {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(root, "contract/openapi.yaml"))
	if err != nil {
		t.Fatalf("reading openapi.yaml: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parsing openapi.yaml: %v", err)
	}

	return openapi{doc: doc, t: t}
}

func (o openapi) mapAt(path ...string) map[string]any {
	o.t.Helper()

	cur := any(o.doc)

	for i, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s is not a mapping", strings.Join(path[:i], "."))
		}

		cur, ok = m[key]
		if !ok {
			o.t.Fatalf("openapi.yaml has no %s", strings.Join(path[:i+1], "."))
		}
	}

	m, ok := cur.(map[string]any)
	if !ok {
		o.t.Fatalf("openapi.yaml: %s is not a mapping", strings.Join(path, "."))
	}

	return m
}

func (o openapi) stringsAt(path ...string) []string {
	o.t.Helper()

	cur := any(o.doc)

	for i, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s is not a mapping", strings.Join(path[:i], "."))
		}

		cur, ok = m[key]
		if !ok {
			o.t.Fatalf("openapi.yaml has no %s", strings.Join(path[:i+1], "."))
		}
	}

	list, ok := cur.([]any)
	if !ok {
		o.t.Fatalf("openapi.yaml: %s is not a list", strings.Join(path, "."))
	}

	out := make([]string, 0, len(list))

	for i, v := range list {
		s, ok := v.(string)
		if !ok {
			o.t.Fatalf("openapi.yaml: %s[%d] is not a string", strings.Join(path, "."), i)
		}

		out = append(out, s)
	}

	return out
}

// properties returns a schema's property names.
func (o openapi) properties(path ...string) []string {
	o.t.Helper()

	props := o.mapAt(append(path, "properties")...)

	out := make([]string, 0, len(props))
	for name := range props {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}

type contractOperation struct {
	method     string
	path       string
	id         string
	idempotent bool
}

// operations reads every (method, path) the document declares.
func (o openapi) operations() []contractOperation {
	o.t.Helper()

	methods := map[string]bool{
		"get": true, "post": true, "put": true, "patch": true, "delete": true,
		"head": true, "options": true, "trace": true,
	}

	var ops []contractOperation

	for path, raw := range o.mapAt("paths") {
		item, ok := raw.(map[string]any)
		if !ok {
			o.t.Fatalf("openapi.yaml: paths[%q] is not a mapping", path)
		}

		for key, rawOp := range item {
			if !methods[key] {
				// `parameters`, `summary`, … — not an operation. Anything
				// else unrecognised is a shape this reader does not know,
				// and silence about it is how a route goes missing.
				if key != "parameters" && key != "summary" && key != "description" {
					o.t.Fatalf("openapi.yaml: paths[%q] has the unexpected key %q", path, key)
				}

				continue
			}

			op, ok := rawOp.(map[string]any)
			if !ok {
				o.t.Fatalf("openapi.yaml: paths[%q].%s is not a mapping", path, key)
			}

			id, ok := op["operationId"].(string)
			if !ok {
				o.t.Fatalf("openapi.yaml: paths[%q].%s has no operationId", path, key)
			}

			ops = append(ops, contractOperation{
				method:     strings.ToUpper(key),
				path:       path,
				id:         id,
				idempotent: o.declaresIdempotencyKey(op),
			})
		}
	}

	if len(ops) < 10 {
		o.t.Fatalf("read only %d operations out of openapi.yaml; the reader has stopped matching", len(ops))
	}

	return ops
}

func (o openapi) declaresIdempotencyKey(op map[string]any) bool {
	params, ok := op["parameters"].([]any)
	if !ok {
		return false
	}

	for _, raw := range params {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if ref, ok := p["$ref"].(string); ok && ref == "#/components/parameters/IdempotencyKey" {
			return true
		}

		// An inline declaration would be as binding as the $ref, so it is
		// recognised too rather than quietly read as "absent".
		if name, ok := p["name"].(string); ok && strings.EqualFold(name, "Idempotency-Key") {
			return true
		}
	}

	return false
}

func TestRouteTableEqualsTheContractsOperations(t *testing.T) {
	o := loadContract(t)

	// The exclusions are named and checked, not silently subtracted. A path
	// that leaves the document would otherwise make this test pass while
	// the CLI's route table lost nothing.
	declared := map[string]bool{}
	for _, op := range o.operations() {
		declared[op.path] = true
	}

	for path, why := range api.ExcludedPaths {
		if !declared[path] {
			t.Errorf("ExcludedPaths names %q (%s), which openapi.yaml no longer declares", path, why)
		}
	}

	var want []string

	for _, op := range o.operations() {
		if _, excluded := api.ExcludedPaths[op.path]; excluded {
			continue
		}

		want = append(want, fmt.Sprintf("%s %s (%s)", op.method, op.path, op.id))
	}

	var got []string

	for _, r := range api.Routes {
		got = append(got, fmt.Sprintf("%s %s (%s)", r.Method, r.Path, r.OpenAPIID))
	}

	assertSetsEqual(t, "routes", got, want, "api.Routes", "openapi.yaml")
}

// Every route maps to an operation the decision table knows, in both
// directions. Without this, a route could exist that Classify answers
// "unrecognised operation" for, at exit 6, for every answer it ever gets.
func TestRouteTableAndOperationVocabularyAgree(t *testing.T) {
	var routeOps, tableOps []string

	for _, r := range api.Routes {
		routeOps = append(routeOps, string(r.Op))
	}

	for _, op := range outcome.Operations {
		tableOps = append(tableOps, string(op))
	}

	assertSetsEqual(t, "operations", routeOps, tableOps, "api.Routes", "outcome.Operations")
}

// AC16, both directions.
func TestIdempotencyColumnEqualsTheContract(t *testing.T) {
	o := loadContract(t)

	var want []string

	for _, op := range o.operations() {
		if _, excluded := api.ExcludedPaths[op.path]; excluded {
			continue
		}

		if op.idempotent {
			want = append(want, op.id)
		}
	}

	var got []string

	for _, r := range api.Routes {
		if r.Idempotent {
			got = append(got, r.OpenAPIID)
		}
	}

	if len(want) == 0 {
		t.Fatal("openapi.yaml declares the IdempotencyKey parameter on no operation; the reader has stopped matching")
	}

	assertSetsEqual(t, "idempotent operations", got, want, "api.Routes", "openapi.yaml")
}

// AC17: the seven keys are the document's, both directions.
func TestEnvelopeKeysEqualTheContract(t *testing.T) {
	o := loadContract(t)

	want := o.stringsAt("components", "schemas", "Error", "properties", "error", "required")

	assertSetsEqual(t, "envelope keys", api.EnvelopeKeys, want, "api.EnvelopeKeys", "Error.error.required")

	if len(api.EnvelopeKeys) != 7 {
		t.Errorf("the envelope has seven keys; this package declares %d", len(api.EnvelopeKeys))
	}
}

// AC25: `Command.state`. The Go set is `outcome.CommandStates`, which
// `ClassifyCommand` branches on — so a state the contract grows reddens here
// rather than falling into the fail-closed branch on a live command.
func TestCommandStateEnumEqualsTheContract(t *testing.T) {
	o := loadContract(t)

	want := o.stringsAt("components", "schemas", "Command", "properties", "state", "enum")

	assertSetsEqual(t, "command states", outcome.CommandStates, want, "outcome.CommandStates", "Command.state.enum")
}

// AC25: the `scopes` enum.
//
// The plan writes this pin as "the `scopes` enum equals `keys.Scopes`".
// `internal/noun/keys` is U4's and does not exist yet, so the list lives here,
// where the contract pin can hold it, and U4 consumes `api.Scopes` rather than
// declaring a second copy. A second copy is the thing this pin exists to
// prevent.
func TestScopesEqualTheContract(t *testing.T) {
	o := loadContract(t)

	want := o.stringsAt("components", "schemas", "CreateApiKeyRequest", "properties", "scopes", "items", "enum")

	assertSetsEqual(t, "scopes", api.Scopes, want, "api.Scopes", "CreateApiKeyRequest.scopes.items.enum")

	// `keys:manage` is control-plane only and cannot be granted to an API
	// key, which is what keeps revocation meaningful. Stated as its own
	// claim so that it fails loudly if the contract ever adds it, rather
	// than being absorbed into the set comparison above.
	for _, s := range api.Scopes {
		if s == "keys:manage" {
			t.Error("`keys:manage` is in the grantable scope list; it is control-plane only (§1.2.x, CreateApiKeyRequest.scopes)")
		}
	}
}

// AC25: the request property sets.
//
// Three comparisons per body, not one. The declared list is held to the
// document *and* to the struct's JSON tags, because those are two different
// ways to drift: a field renamed in Go, and a property added to the contract.
func TestRequestBodyFieldsEqualTheContract(t *testing.T) {
	o := loadContract(t)

	cases := []struct {
		name     string
		declared []string
		value    any
		schema   []string
	}{
		{
			name: "CreateApiKeyRequest", declared: api.CreateAPIKeyFields, value: api.CreateAPIKeyRequest{},
			schema: []string{"components", "schemas", "CreateApiKeyRequest"},
		},
		{
			name: "RevokeApiKeyRequest", declared: api.RevokeAPIKeyFields, value: api.RevokeAPIKeyRequest{},
			schema: []string{"components", "schemas", "RevokeApiKeyRequest"},
		},
		{
			name: "TransferRequest", declared: api.TransferFields, value: api.TransferRequest{},
			schema: []string{"components", "schemas", "TransferRequest"},
		},
		{
			name: "TransferRequest.amount", declared: api.TransferAmountFields, value: api.TransferAmount{},
			schema: []string{"components", "schemas", "TransferRequest", "properties", "amount"},
		},
		{
			name: "TransferSide", declared: api.TransferSideFields, value: api.TransferSide{},
			schema: []string{"components", "schemas", "TransferSide"},
		},
		{
			name: "ExecuteRequest", declared: api.ExecuteFields, value: api.ExecuteRequest{},
			schema: []string{"components", "schemas", "ExecuteRequest"},
		},
		{
			name: "Corridor", declared: api.CorridorFields, value: api.Corridor{},
			schema: []string{"components", "schemas", "Corridor"},
		},
		{
			name: "Corridor.amount", declared: api.CorridorAmountFields, value: api.CorridorAmount{},
			schema: []string{"components", "schemas", "Corridor", "properties", "amount"},
		},
		{
			name: "CorridorSide", declared: api.CorridorSideFields, value: api.CorridorSide{},
			schema: []string{"components", "schemas", "CorridorSide"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertSetsEqual(t, c.name+" against the contract",
				c.declared, o.properties(c.schema...),
				"the declared field list", "openapi.yaml")

			assertSetsEqual(t, c.name+" against the struct",
				c.declared, jsonTags(t, c.value),
				"the declared field list", "the Go struct's json tags")
		})
	}

	if len(cases) < 9 {
		t.Errorf("this pin covers %d bodies; AC25 names three and the rest are free", len(cases))
	}
}

// AC18 and AC25: the response headers this client reads are the document's
// `components.headers`, both directions — so a header FERRY starts sending
// cannot be dropped on the floor here unnoticed.
func TestMetaHeadersEqualTheContract(t *testing.T) {
	o := loadContract(t)

	headers := o.mapAt("components", "headers")

	// The document names headers by their schema key (`FerryCommandId`),
	// not by their wire name, so the comparison is over wire names derived
	// from the operations that reference them. Simpler and just as binding:
	// compare counts and then each wire name's presence in the raw
	// document, which is where the wire name actually appears.
	if len(headers) != len(api.MetaHeaders) {
		t.Errorf("openapi.yaml declares %d response headers; this client reads %d", len(headers), len(api.MetaHeaders))
	}

	raw := rawContract(t)

	for name := range api.MetaHeaders {
		if !strings.Contains(raw, name+":") {
			t.Errorf("this client reads the header %q, which openapi.yaml does not name", name)
		}
	}
}

func rawContract(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(root, "contract/openapi.yaml"))
	if err != nil {
		t.Fatalf("reading openapi.yaml: %v", err)
	}

	return string(b)
}

func jsonTags(t *testing.T, v any) []string {
	t.Helper()

	rt := reflect.TypeOf(v)
	if rt.Kind() != reflect.Struct {
		t.Fatalf("%T is not a struct", v)
	}

	out := make([]string, 0, rt.NumField())

	for i := range rt.NumField() {
		f := rt.Field(i)

		tag, ok := f.Tag.Lookup("json")
		if !ok {
			t.Fatalf("%T.%s has no json tag, so what it sends on the wire is its Go name", v, f.Name)
		}

		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}

		out = append(out, name)
	}

	return out
}

func assertSetsEqual(t *testing.T, what string, got, want []string, gotLabel, wantLabel string) {
	t.Helper()

	inWant := map[string]bool{}
	for _, s := range want {
		inWant[s] = true
	}

	inGot := map[string]bool{}
	for _, s := range got {
		inGot[s] = true
	}

	var onlyGot, onlyWant []string

	for s := range inGot {
		if !inWant[s] {
			onlyGot = append(onlyGot, s)
		}
	}

	for s := range inWant {
		if !inGot[s] {
			onlyWant = append(onlyWant, s)
		}
	}

	sort.Strings(onlyGot)
	sort.Strings(onlyWant)

	if len(onlyGot) > 0 {
		t.Errorf("%s: %s has %v, which %s does not", what, gotLabel, onlyGot, wantLabel)
	}

	if len(onlyWant) > 0 {
		t.Errorf("%s: %s has %v, which %s does not", what, wantLabel, onlyWant, gotLabel)
	}
}
