// Package expression is the typed predicate language shared by policies and query filters.
package expression

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lightshipHQ/lightship/internal/schema"
)

type Profile string

const (
	PolicyProfile Profile = "policy"
	FilterProfile Profile = "filter"
)

type Operator string

const (
	And        Operator = "and"
	Or         Operator = "or"
	Not        Operator = "not"
	True       Operator = "true"
	False      Operator = "false"
	Equal      Operator = "=="
	NotEqual   Operator = "!="
	In         Operator = "in"
	StartsWith Operator = "startsWith"
	Exists     Operator = "exists"
	HasAny     Operator = "hasAny"
	HasAll     Operator = "hasAll"
	Greater    Operator = ">"
	GreaterEq  Operator = ">="
	Less       Operator = "<"
	LessEq     Operator = "<="
	Between    Operator = "between"
)

type Value struct {
	Literal any
	Attr    string
}

type Expr struct {
	Op       Operator
	Field    schema.Field
	Value    Value
	Values   []Value
	Children []Expr
}

// Validate applies the capability profile independently of parsing or SQL generation.
func Validate(e Expr, profile Profile) error {
	if e.Op == And || e.Op == Or || e.Op == Not {
		if (e.Op == Not && len(e.Children) != 1) || (e.Op != Not && len(e.Children) < 1) {
			return fmt.Errorf("invalid %s expression", e.Op)
		}
		for _, child := range e.Children {
			if err := Validate(child, profile); err != nil {
				return err
			}
		}
		return nil
	}
	if e.Op == True || e.Op == False {
		return nil
	}

	t := e.Field.Type()
	allowed := false
	switch t {
	case schema.TypeString:
		allowed = e.Op == Equal || e.Op == NotEqual || e.Op == In
		if profile == FilterProfile {
			allowed = allowed || e.Op == StartsWith || e.Op == Exists
		}
	case schema.TypeStringArray:
		allowed = e.Op == In
		if profile == FilterProfile {
			allowed = allowed || e.Op == HasAny || e.Op == HasAll
		}
	case schema.TypeBoolean:
		allowed = e.Op == Equal || e.Op == NotEqual
	case schema.TypeNumber:
		allowed = profile == FilterProfile && (e.Op == Equal || e.Op == NotEqual ||
			e.Op == Greater || e.Op == GreaterEq || e.Op == Less || e.Op == LessEq || e.Op == Between)
	}
	if !allowed {
		return fmt.Errorf("%s is not available for %s field %s in the %s profile",
			e.Op, t, e.Field.Ref(), profile)
	}
	return validateValues(e, t)
}

func validateValues(e Expr, t schema.LogicalType) error {
	want := 1
	if e.Op == Exists {
		want = 0
	} else if e.Op == In && t == schema.TypeString || e.Op == HasAny || e.Op == HasAll {
		if len(e.Values) == 0 {
			return fmt.Errorf("%s needs a non-empty values list", e.Op)
		}
		want = -1
	} else if e.Op == Between {
		if len(e.Values) != 2 {
			return fmt.Errorf("between needs exactly two values")
		}
		want = -1
	}
	if want == 0 && (e.Value.Literal != nil || e.Value.Attr != "" || len(e.Values) != 0) {
		return fmt.Errorf("%s takes no value", e.Op)
	}
	if want == 1 {
		return validateValue(e.Value, elementType(t))
	}
	for _, v := range e.Values {
		if err := validateValue(v, elementType(t)); err != nil {
			return err
		}
	}
	return nil
}

func elementType(t schema.LogicalType) schema.LogicalType {
	if t == schema.TypeStringArray {
		return schema.TypeString
	}
	return t
}

func validateValue(v Value, t schema.LogicalType) error {
	if v.Attr != "" {
		if t != schema.TypeString {
			return fmt.Errorf("caller attributes can only be used as string values")
		}
		return nil
	}
	ok := false
	switch t {
	case schema.TypeString:
		_, ok = v.Literal.(string)
	case schema.TypeBoolean:
		_, ok = v.Literal.(bool)
	case schema.TypeNumber:
		switch v.Literal.(type) {
		case int64, uint64, float64:
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("value for %s must be a %s", t, t)
	}
	return nil
}

// Query is a parameterized SQL predicate.
type Query struct {
	SQL  string
	Args []any
}

var ErrUnsatisfiable = errors.New("expression references a caller attribute this caller does not have")

func Compile(e Expr, profile Profile, attrs map[string]string) (Query, error) {
	if err := Validate(e, profile); err != nil {
		return Query{}, err
	}
	b := compiler{attrs: attrs}
	if err := b.emit(e, false); err != nil {
		return Query{}, err
	}
	return Query{SQL: b.sql.String(), Args: b.args}, nil
}

type compiler struct {
	sql   strings.Builder
	args  []any
	attrs map[string]string
}

func (b *compiler) emit(e Expr, negated bool) error {
	switch e.Op {
	case True, False:
		v := e.Op == True
		if negated {
			v = !v
		}
		if v {
			b.sql.WriteString("1")
		} else {
			b.sql.WriteString("0")
		}
		return nil
	case Not:
		return b.emit(e.Children[0], !negated)
	case And, Or:
		op := " AND "
		if (e.Op == Or) != negated {
			op = " OR "
		}
		b.sql.WriteString("(")
		for i, child := range e.Children {
			if i > 0 {
				b.sql.WriteString(op)
			}
			if err := b.emit(child, negated); err != nil {
				return err
			}
		}
		b.sql.WriteString(")")
		return nil
	case Exists:
		if e.Field.Map == "" {
			if negated {
				b.sql.WriteString("0")
			} else {
				b.sql.WriteString("1")
			}
			return nil
		}
		if negated {
			b.sql.WriteString("(NOT mapContains(")
		} else {
			b.sql.WriteString("mapContains(")
		}
		fmt.Fprintf(&b.sql, "%s, ?)", e.Field.Map)
		if negated {
			b.sql.WriteString(")")
		}
		b.args = append(b.args, e.Field.Name)
		return nil
	}
	return b.leaf(e, negated)
}

func (b *compiler) leaf(e Expr, negated bool) error {
	guard := e.Field.Map != ""
	if guard {
		fmt.Fprintf(&b.sql, "(mapContains(%s, ?) AND ", e.Field.Map)
		b.args = append(b.args, e.Field.Name)
	} else {
		b.sql.WriteString("(")
	}
	field := e.Field.Name
	if e.Field.Map != "" {
		field = e.Field.Map + "[?]"
		b.args = append(b.args, e.Field.Name)
	}
	switch e.Field.Type() {
	case schema.TypeBoolean:
		if e.Field.Map != "" {
			field = "toBoolOrNull(" + field + ")"
		}
	case schema.TypeNumber:
		if e.Field.Map != "" {
			field = "toFloat64OrNull(" + field + ")"
		}
	case schema.TypeStringArray:
		if e.Field.Map != "" {
			field = "JSONExtract(" + field + ", 'Array(String)')"
		}
	}

	op := e.Op
	if negated {
		switch op {
		case Equal:
			op = NotEqual
		case NotEqual:
			op = Equal
		case Greater:
			op = LessEq
		case GreaterEq:
			op = Less
		case Less:
			op = GreaterEq
		case LessEq:
			op = Greater
		}
	}
	switch op {
	case Equal, NotEqual, Greater, GreaterEq, Less, LessEq:
		cmp := map[Operator]string{Equal: "=", NotEqual: "<>", Greater: ">", GreaterEq: ">=", Less: "<", LessEq: "<="}[op]
		v, err := b.value(e.Value)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b.sql, "%s %s ?", field, cmp)
		b.args = append(b.args, v)
	case StartsWith:
		v, err := b.value(e.Value)
		if err != nil {
			return err
		}
		if negated {
			b.sql.WriteString("NOT ")
		}
		fmt.Fprintf(&b.sql, "startsWith(%s, ?)", field)
		b.args = append(b.args, v)
	case In:
		if e.Field.Type() == schema.TypeStringArray {
			v, err := b.value(e.Value)
			if err != nil {
				return err
			}
			if negated {
				b.sql.WriteString("NOT ")
			}
			fmt.Fprintf(&b.sql, "has(%s, ?)", field)
			b.args = append(b.args, v)
		} else {
			if negated {
				b.sql.WriteString(field + " NOT IN (")
			} else {
				b.sql.WriteString(field + " IN (")
			}
			if err := b.values(e.Values); err != nil {
				return err
			}
			b.sql.WriteString(")")
		}
	case HasAny, HasAll:
		if negated {
			b.sql.WriteString("NOT ")
		}
		fmt.Fprintf(&b.sql, "%s(%s, [", op, field)
		if err := b.values(e.Values); err != nil {
			return err
		}
		b.sql.WriteString("])")
	case Between:
		if negated {
			b.sql.WriteString("NOT ")
		}
		fmt.Fprintf(&b.sql, "(%s BETWEEN ? AND ?)", field)
		for _, raw := range e.Values {
			v, err := b.value(raw)
			if err != nil {
				return err
			}
			b.args = append(b.args, v)
		}
	default:
		return fmt.Errorf("cannot compile operator %s", e.Op)
	}
	b.sql.WriteString(")")
	return nil
}

func (b *compiler) values(vs []Value) error {
	for i, raw := range vs {
		if i > 0 {
			b.sql.WriteString(", ")
		}
		v, err := b.value(raw)
		if err != nil {
			return err
		}
		b.sql.WriteString("?")
		b.args = append(b.args, v)
	}
	return nil
}

func (b *compiler) value(v Value) (any, error) {
	if v.Attr == "" {
		return v.Literal, nil
	}
	got, ok := b.attrs[v.Attr]
	if !ok {
		return nil, fmt.Errorf("%w: user.%s", ErrUnsatisfiable, v.Attr)
	}
	return got, nil
}

func Operators(t schema.LogicalType, profile Profile) []Operator {
	candidates := []Operator{Equal, NotEqual, In, StartsWith, Exists, HasAny, HasAll, Greater, GreaterEq, Less, LessEq, Between}
	var out []Operator
	for _, op := range candidates {
		e := Expr{Op: op, Field: schema.Field{Name: "field", LogicalType: t}, Value: sampleValue(elementType(t)), Values: []Value{sampleValue(elementType(t)), sampleValue(elementType(t))}}
		if op == Exists {
			e.Value, e.Values = Value{}, nil
		}
		if Validate(e, profile) == nil {
			out = append(out, op)
		}
	}
	return out
}

func sampleValue(t schema.LogicalType) Value {
	switch t {
	case schema.TypeBoolean:
		return Value{Literal: true}
	case schema.TypeNumber:
		return Value{Literal: float64(1)}
	default:
		return Value{Literal: "x"}
	}
}
