package filter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/lightshipHQ/lightship/internal/schema"
)

func typedModel() schema.Model {
	return schema.Model{Fields: []schema.Field{
		{Name: "ok", LogicalType: schema.TypeBoolean, Filterable: true},
		{Name: "duration", LogicalType: schema.TypeNumber, Filterable: true},
		{Map: "Attrs", Name: "tags", LogicalType: schema.TypeStringArray, Filterable: true},
	}}
}

func TestJSONWireLowersToTypedSharedExpressions(t *testing.T) {
	var f Filter
	err := json.Unmarshal([]byte(`{"conditions":[
		{"name":"ok","op":"eq","value":true},
		{"name":"duration","op":"between","values":[10,20]},
		{"map":"Attrs","name":"tags","op":"has_all","values":["pii","reviewed"]}
	]}`), &f)
	if err != nil {
		t.Fatal(err)
	}
	q, err := f.Build(typedModel())
	if err != nil {
		t.Fatal(err)
	}
	want := []any{true, float64(10), float64(20), "tags", "tags", "pii", "reviewed"}
	if !reflect.DeepEqual(q.Args, want) {
		t.Fatalf("args=%#v want %#v", q.Args, want)
	}
}

func TestWireTypeMismatchIsRejected(t *testing.T) {
	f := Filter{Conditions: []Condition{{Name: "duration", Op: "gt", Value: "ten"}}}
	if _, err := f.Build(typedModel()); err == nil {
		t.Fatal("string value for number field was accepted")
	}
}

func TestBoundFieldUsesTheSameFilterConfigurationAsOtherFields(t *testing.T) {
	m := schema.Model{
		Binding: schema.Binding{TraceID: "TraceId"},
		Fields:  []schema.Field{{Name: "TraceId", Filterable: true}},
	}
	f := Filter{Conditions: []Condition{{Name: "TraceId", Op: "eq", Value: "trace-123"}}}
	q, err := f.Build(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q.SQL, "TraceId") || !reflect.DeepEqual(q.Args, []any{"trace-123"}) {
		t.Fatalf("query=%q args=%#v", q.SQL, q.Args)
	}
}

func TestBoundFieldIsNotImplicitlyFilterable(t *testing.T) {
	m := schema.Model{Binding: schema.Binding{TraceID: "TraceId"}}
	f := Filter{Conditions: []Condition{{Name: "TraceId", Op: "eq", Value: "trace-123"}}}
	if _, err := f.Build(m); err == nil {
		t.Fatal("unconfigured bound field was accepted as a filter")
	}
}

func TestLegacyCanonicalizationIsStable(t *testing.T) {
	var f Filter
	if err := json.Unmarshal([]byte(`{"conditions":[{"map":"A","name":"x","op":"in","values":["b","a"]},{"name":"S","op":"eq","value":"v"}]}`), &f); err != nil {
		t.Fatal(err)
	}
	want := "\x00S\x00eq\x00v\x00\x02A\x00x\x00in\x00\x00a\x01b"
	if got := f.Canonical(); got != want {
		t.Fatalf("canonical=%q want %q", got, want)
	}
}

func TestOperatorsAreTyped(t *testing.T) {
	if got := OperatorsFor(schema.TypeStringArray); !reflect.DeepEqual(got, []string{"has", "has_all", "has_any"}) {
		t.Fatalf("array operators=%v", got)
	}
	if got := OperatorsFor(schema.TypeNumber); !reflect.DeepEqual(got, []string{"between", "eq", "gt", "gte", "lt", "lte", "ne"}) {
		t.Fatalf("number operators=%v", got)
	}
}
