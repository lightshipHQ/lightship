package expression

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lightshipHQ/lightship/internal/schema"
)

func field(t schema.LogicalType, mapCol string) schema.Field {
	return schema.Field{Map: mapCol, Name: "value", LogicalType: t}
}

func TestCapabilityProfiles(t *testing.T) {
	cases := []struct {
		name           string
		typ            schema.LogicalType
		op             Operator
		policy, filter bool
	}{
		{"string equality", schema.TypeString, Equal, true, true},
		{"string prefix", schema.TypeString, StartsWith, false, true},
		{"string exists", schema.TypeString, Exists, false, true},
		{"array membership", schema.TypeStringArray, In, true, true},
		{"array any", schema.TypeStringArray, HasAny, false, true},
		{"array all", schema.TypeStringArray, HasAll, false, true},
		{"boolean equality", schema.TypeBoolean, Equal, true, true},
		{"number equality", schema.TypeNumber, Equal, false, true},
		{"number range", schema.TypeNumber, GreaterEq, false, true},
		{"number between", schema.TypeNumber, Between, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Expr{Op: tc.op, Field: field(tc.typ, "")}
			switch tc.op {
			case Exists:
			case In:
				if tc.typ == schema.TypeString {
					e.Values = []Value{{Literal: "x"}}
				} else {
					e.Value = Value{Literal: "x"}
				}
			case HasAny, HasAll:
				e.Values = []Value{{Literal: "x"}}
			case Between:
				e.Values = []Value{{Literal: float64(1)}, {Literal: float64(2)}}
			default:
				e.Value = sampleValue(elementType(tc.typ))
			}
			if got := Validate(e, PolicyProfile) == nil; got != tc.policy {
				t.Errorf("policy enabled=%v, want %v", got, tc.policy)
			}
			if got := Validate(e, FilterProfile) == nil; got != tc.filter {
				t.Errorf("filter enabled=%v, want %v", got, tc.filter)
			}
		})
	}
}

func TestTypedSQLAndBindings(t *testing.T) {
	cases := []struct {
		name     string
		expr     Expr
		wantSQL  string
		wantArgs []any
	}{
		{"boolean map", Expr{Op: Equal, Field: field(schema.TypeBoolean, "Attrs"), Value: Value{Literal: true}},
			"(mapContains(Attrs, ?) AND toBoolOrNull(Attrs[?]) = ?)", []any{"value", "value", true}},
		{"number between", Expr{Op: Between, Field: field(schema.TypeNumber, ""), Values: []Value{{Literal: float64(1)}, {Literal: float64(2)}}},
			"((value BETWEEN ? AND ?))", []any{float64(1), float64(2)}},
		{"native array membership", Expr{Op: In, Field: field(schema.TypeStringArray, ""), Value: Value{Literal: "pii"}},
			"(has(value, ?))", []any{"pii"}},
		{"serialized map array any", Expr{Op: HasAny, Field: field(schema.TypeStringArray, "Attrs"), Values: []Value{{Literal: "pii"}, {Literal: "secret"}}},
			"(mapContains(Attrs, ?) AND hasAny(JSONExtract(Attrs[?], 'Array(String)'), [?, ?]))", []any{"value", "value", "pii", "secret"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Compile(tc.expr, FilterProfile, nil)
			if err != nil {
				t.Fatal(err)
			}
			if q.SQL != tc.wantSQL {
				t.Errorf("SQL\n got: %s\nwant: %s", q.SQL, tc.wantSQL)
			}
			if !reflect.DeepEqual(q.Args, tc.wantArgs) {
				t.Errorf("args=%#v want %#v", q.Args, tc.wantArgs)
			}
		})
	}
}

func TestNegationKeepsMissingMapGuardOutside(t *testing.T) {
	leaf := Expr{Op: In, Field: field(schema.TypeStringArray, "Attrs"), Value: Value{Literal: "pii"}}
	q, err := Compile(Expr{Op: Not, Children: []Expr{leaf}}, PolicyProfile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(q.SQL, "(mapContains(") || !strings.Contains(q.SQL, "NOT has(") {
		t.Fatalf("guard or negation misplaced: %s", q.SQL)
	}
}
