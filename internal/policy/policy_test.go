package policy

import (
	"strings"
	"testing"

	"github.com/lightshipHQ/lightship/internal/schema"
)

func testEnv(t *testing.T) *Env {
	t.Helper()
	e, err := NewEnv(schema.Model{
		UserAttrs: []string{"tenant_id"},
		Fields: []schema.Field{
			{Map: "ResourceAttributes", Name: "tenant.id", Policy: true},
			{Map: "ResourceAttributes", Name: "env", Policy: true},
			{Map: "SpanAttributes", Name: "agent.id", Policy: true},
			{Name: "ServiceName", Policy: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func translate(t *testing.T, src string) Query {
	t.Helper()
	e := testEnv(t)
	p, err := e.Compile(src)
	if err != nil {
		t.Fatalf("compile %q: %v", src, err)
	}
	q, err := e.Translate(p, map[string]string{"tenant_id": "acme"})
	if err != nil {
		t.Fatalf("translate %q: %v", src, err)
	}
	return q
}

func TestGoldenSQL(t *testing.T) {
	cases := []struct{ name, src, sql string }{
		{"equality against a caller attribute",
			`ResourceAttributes["tenant.id"] == user.tenant_id`,
			`(mapContains(ResourceAttributes, ?) AND ResourceAttributes[?] = ?)`},
		{"inequality",
			`ResourceAttributes["env"] != "prod"`,
			`(mapContains(ResourceAttributes, ?) AND ResourceAttributes[?] <> ?)`},
		{"membership",
			`SpanAttributes["agent.id"] in ["a", "b"]`,
			`(mapContains(SpanAttributes, ?) AND SpanAttributes[?] IN (?, ?))`},
		{"conjunction",
			`ResourceAttributes["env"] == "prod" && SpanAttributes["agent.id"] == "a"`,
			`((mapContains(ResourceAttributes, ?) AND ResourceAttributes[?] = ?) AND ` +
				`(mapContains(SpanAttributes, ?) AND SpanAttributes[?] = ?))`},
		{"literal true is the admin bypass", `true`, `1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := translate(t, tc.src).SQL; got != tc.sql {
				t.Errorf("\n got: %s\nwant: %s", got, tc.sql)
			}
		})
	}
}

// The reason negation is normalised to the leaves. If NOT were emitted around the guarded leaf,
// a span that never set `env` would satisfy `!(env == "prod")` and absence would grant access.
func TestNegationKeepsTheGuardOutside(t *testing.T) {
	direct := translate(t, `ResourceAttributes["env"] != "prod"`).SQL
	negated := translate(t, `!(ResourceAttributes["env"] == "prod")`).SQL
	if direct != negated {
		t.Errorf("negation should normalise to the same SQL:\n  !=  %s\n  !() %s", direct, negated)
	}
	if !strings.HasPrefix(negated, "(mapContains(") {
		t.Errorf("guard must sit outside the negation, got: %s", negated)
	}
	if strings.Contains(negated, "NOT (mapContains") {
		t.Error("guard was negated: a span missing the key would satisfy this predicate")
	}
}

func TestDeMorgan(t *testing.T) {
	got := translate(t, `!(ResourceAttributes["env"] == "prod" && SpanAttributes["agent.id"] == "a")`).SQL
	want := `((mapContains(ResourceAttributes, ?) AND ResourceAttributes[?] <> ?) OR ` +
		`(mapContains(SpanAttributes, ?) AND SpanAttributes[?] <> ?))`
	if got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

func TestNegatedMembership(t *testing.T) {
	got := translate(t, `!(SpanAttributes["agent.id"] in ["a"])`).SQL
	if !strings.Contains(got, "NOT IN") || !strings.HasPrefix(got, "(mapContains(") {
		t.Errorf("want a guarded NOT IN, got: %s", got)
	}
}

// Values are bound, never interpolated. Only column names and attribute keys reach the SQL text,
// and both come from the validated config.
func TestValuesAreBoundNotInterpolated(t *testing.T) {
	q := translate(t, `ResourceAttributes["env"] == "'; drop table otel_traces --"`)
	if strings.Contains(q.SQL, "drop table") {
		t.Fatalf("value reached the SQL text: %s", q.SQL)
	}
	found := false
	for _, a := range q.Args {
		if a == "'; drop table otel_traces --" {
			found = true
		}
	}
	if !found {
		t.Error("value should be present as a bound argument")
	}
}

func TestRejected(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"undeclared field", `ResourceAttributes["nope"] == "x"`, "not a field a policy may reference"},
		{"undeclared caller attribute", `ResourceAttributes["env"] == user.nope`, "not in user_attributes"},
		{"integer literal", `ResourceAttributes["env"] == 1`, "no matching overload"},
		{"function call", `size(ResourceAttributes) == 1`, "must be a declared field"},
		// A column nobody marked is not vocabulary at all: CEL rejects it before the whitelist does.
		{"unmarked column", `Nope == "x"`, "undeclared reference"},
		{"dotted member access", `ResourceAttributes.env == "x"`, `write ResourceAttributes["key"]`},
		{"field compared to field", `ResourceAttributes["env"] == SpanAttributes["agent.id"]`, "string literal or user."},
		{"bare field reference", `ResourceAttributes["env"]`, "not a policy on its own"},
		{"macro", `["a"].exists(x, x == "a")`, "undeclared reference"},
		{"arithmetic", `ResourceAttributes["env"] == "a" + "b"`, "string literal or user."},
		{"wrong scope", `trace["env"] == "x"`, "undeclared reference"},
	}
	e := testEnv(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.Compile(tc.src)
			if err == nil {
				t.Fatalf("expected %q to be rejected", tc.src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

// A caller missing an attribute the policy needs contributes nothing, rather than denying what
// another of their roles granted.
func TestMissingCallerAttributeIsUnsatisfiable(t *testing.T) {
	e := testEnv(t)
	p, err := e.Compile(`ResourceAttributes["tenant.id"] == user.tenant_id`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Translate(p, map[string]string{}); err == nil {
		t.Fatal("want ErrUnsatisfiable")
	}
}

func TestNoUsablePolicyDeniesWithoutQuerying(t *testing.T) {
	e := testEnv(t)
	p, _ := e.Compile(`ResourceAttributes["tenant.id"] == user.tenant_id`)
	_, err := e.Predicate([]*Program{p}, map[string]string{})
	if err != ErrUnsatisfiable {
		t.Fatalf("want ErrUnsatisfiable, got %v", err)
	}
}

func TestRowsIsTwoPhaseWithAnyQuantifier(t *testing.T) {
	e := testEnv(t)
	p, _ := e.Compile(`ResourceAttributes["env"] == "prod"`)
	pred, err := e.Predicate([]*Program{p}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	q := Having(pred)
	for _, want := range []string{"countIf(", ") > 0"} {
		if !strings.Contains(q.SQL, want) {
			t.Errorf("missing %q in:\n%s", want, q.SQL)
		}
	}
}

func TestTypedPolicyExpressions(t *testing.T) {
	e, err := NewEnv(schema.Model{Fields: []schema.Field{
		{Name: "Allowed", LogicalType: schema.TypeBoolean, Policy: true},
		{Map: "SpanAttributes", Name: "tags", LogicalType: schema.TypeStringArray, Policy: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{`Allowed == true`, `"label:pii" in SpanAttributes["tags"]`} {
		p, err := e.Compile(src)
		if err != nil {
			t.Fatalf("compile %s: %v", src, err)
		}
		q, err := e.Translate(p, nil)
		if err != nil {
			t.Fatalf("translate %s: %v", src, err)
		}
		if q.SQL == "" {
			t.Fatalf("empty SQL for %s", src)
		}
	}
}

func TestPolicyProfileRejectsBroaderOperations(t *testing.T) {
	e, err := NewEnv(schema.Model{Fields: []schema.Field{{Name: "ServiceName", Policy: true}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{`ServiceName.startsWith("api-")`, `exists(ServiceName)`} {
		if _, err := e.Compile(src); err == nil {
			t.Fatalf("policy profile accepted %s", src)
		}
	}
}
