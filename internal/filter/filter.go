// Package filter adapts the stable structured JSON filter wire format to the shared expression DSL.
// Policies and filters both use ANY-span matching and return selected traces whole. A filter only
// narrows the authorized trace set; its ANDed conditions must match together on at least one span.
package filter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lightshipHQ/lightship/internal/expression"
	"github.com/lightshipHQ/lightship/internal/policy"
	"github.com/lightshipHQ/lightship/internal/schema"
)

const (
	MaxConditions = 20
	MaxValues     = 100
)

type Condition struct {
	Map    string `json:"map,omitempty"`
	Name   string `json:"name"`
	Op     string `json:"op"`
	Value  any    `json:"value,omitempty"`
	Values []any  `json:"values,omitempty"`
}

type Filter struct {
	Conditions []Condition `json:"conditions"`
}

func (f Filter) Empty() bool { return len(f.Conditions) == 0 }

var wireOps = map[string]expression.Operator{
	"eq": expression.Equal, "ne": expression.NotEqual, "in": expression.In,
	"prefix": expression.StartsWith, "exists": expression.Exists,
	"has": expression.In, "has_any": expression.HasAny, "has_all": expression.HasAll,
	"gt": expression.Greater, "gte": expression.GreaterEq, "lt": expression.Less,
	"lte": expression.LessEq, "between": expression.Between,
}

func Operators() []string {
	out := make([]string, 0, len(wireOps)+2)
	for op := range wireOps {
		out = append(out, op)
	}
	out = append(out, "not_in", "not_exists")
	sort.Strings(out)
	return out
}

func OperatorsFor(t schema.LogicalType) []string {
	var out []string
	for _, op := range Operators() {
		if t == schema.TypeStringArray && (op == "in" || op == "not_in") {
			continue
		}
		if t != schema.TypeStringArray && op == "has" {
			continue
		}
		base := op
		if op == "not_in" {
			base = "in"
		}
		if op == "not_exists" {
			base = "exists"
		}
		probe := expression.Expr{Op: wireOps[base], Field: schema.Field{Name: "field", LogicalType: t}, Value: sample(t), Values: []expression.Value{sample(elementType(t)), sample(elementType(t))}}
		if probe.Op == expression.Exists {
			probe.Value, probe.Values = expression.Value{}, nil
		}
		if expression.Validate(probe, expression.FilterProfile) == nil {
			out = append(out, op)
		}
	}
	return out
}

func (f Filter) Build(m schema.Model) (policy.Query, error) {
	if len(f.Conditions) > MaxConditions {
		return policy.Query{}, fmt.Errorf("at most %d conditions, got %d", MaxConditions, len(f.Conditions))
	}
	allowed := map[string]schema.Field{}
	for _, field := range m.Filterable() {
		allowed[field.Key()] = field
	}
	root := expression.Expr{Op: expression.And}
	for _, c := range f.Conditions {
		field, ok := allowed[schema.Field{Map: c.Map, Name: c.Name}.Key()]
		if !ok {
			return policy.Query{}, fmt.Errorf("%s is not a filterable field", schema.Field{Map: c.Map, Name: c.Name}.Ref())
		}
		e, err := c.expression(field)
		if err != nil {
			return policy.Query{}, err
		}
		root.Children = append(root.Children, e)
	}
	if len(root.Children) == 0 {
		return policy.Query{SQL: "1"}, nil
	}
	return expression.Compile(root, expression.FilterProfile, nil)
}

func (c Condition) expression(field schema.Field) (expression.Expr, error) {
	base := c.Op
	negated := false
	if base == "not_in" {
		base, negated = "in", true
	}
	if base == "not_exists" {
		base, negated = "exists", true
	}
	op, ok := wireOps[base]
	if !ok {
		return expression.Expr{}, fmt.Errorf("unknown operator %q: one of %s", c.Op, strings.Join(Operators(), ", "))
	}
	e := expression.Expr{Op: op, Field: field}

	usesValues := op == expression.HasAny || op == expression.HasAll || op == expression.Between || (op == expression.In && field.Type() == schema.TypeString)
	usesValue := !usesValues && op != expression.Exists
	if op == expression.In && field.Type() == schema.TypeStringArray && c.Op != "has" {
		return e, fmt.Errorf("string_array membership uses operator has")
	}
	if c.Op == "has" && field.Type() != schema.TypeStringArray {
		return e, fmt.Errorf("has is only available for string_array fields")
	}
	if c.Op == "prefix" && c.Value == "" {
		return e, fmt.Errorf("prefix needs a value")
	}
	if usesValues {
		if c.Value != nil {
			return e, fmt.Errorf("%s takes values, not value", c.Op)
		}
		if len(c.Values) == 0 {
			return e, fmt.Errorf("%s needs a non-empty values list", c.Op)
		}
		if len(c.Values) > MaxValues {
			return e, fmt.Errorf("%s takes at most %d values, got %d", c.Op, MaxValues, len(c.Values))
		}
		for _, raw := range c.Values {
			v, err := value(raw, elementType(field.Type()))
			if err != nil {
				return e, fmt.Errorf("%s: %w", field.Ref(), err)
			}
			e.Values = append(e.Values, v)
		}
	} else if usesValue {
		if len(c.Values) != 0 {
			return e, fmt.Errorf("%s takes value, not values", c.Op)
		}
		v, err := value(c.Value, elementType(field.Type()))
		if err != nil {
			return e, fmt.Errorf("%s: %w", field.Ref(), err)
		}
		e.Value = v
	} else if c.Value != nil || len(c.Values) != 0 {
		return e, fmt.Errorf("%s takes no value", c.Op)
	}
	if err := expression.Validate(e, expression.FilterProfile); err != nil {
		return e, err
	}
	if negated {
		e = expression.Expr{Op: expression.Not, Children: []expression.Expr{e}}
	}
	return e, nil
}

func value(raw any, t schema.LogicalType) (expression.Value, error) {
	if raw == nil {
		return expression.Value{}, fmt.Errorf("value is required")
	}
	v := expression.Value{Literal: raw}
	probe := expression.Expr{Op: expression.Equal, Field: schema.Field{Name: "value", LogicalType: t}, Value: v}
	if err := expression.Validate(probe, expression.FilterProfile); err != nil {
		return v, fmt.Errorf("value must be a %s", t)
	}
	return v, nil
}

func elementType(t schema.LogicalType) schema.LogicalType {
	if t == schema.TypeStringArray {
		return schema.TypeString
	}
	return t
}
func sample(t schema.LogicalType) expression.Value {
	switch t {
	case schema.TypeBoolean:
		return expression.Value{Literal: true}
	case schema.TypeNumber:
		return expression.Value{Literal: float64(1)}
	default:
		return expression.Value{Literal: "x"}
	}
}

// Canonical intentionally remains based on the wire representation. This preserves hashes already
// embedded in cursors while still making condition and set ordering irrelevant.
func (f Filter) Canonical() string {
	parts := make([]string, 0, len(f.Conditions))
	for _, c := range f.Conditions {
		vs := make([]string, 0, len(c.Values))
		for _, v := range c.Values {
			vs = append(vs, canonicalValue(v))
		}
		sort.Strings(vs)
		parts = append(parts, fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", c.Map, c.Name, c.Op, canonicalValue(c.Value), strings.Join(vs, "\x01")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x02")
}

func canonicalValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
