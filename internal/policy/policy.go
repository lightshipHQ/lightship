// Package policy parses the shared expression DSL from CEL policy text.
package policy

import (
	"errors"
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"github.com/lightshipHQ/lightship/internal/expression"
	"github.com/lightshipHQ/lightship/internal/schema"
)

var ErrUnsatisfiable = errors.New("policy references a caller attribute this caller does not have")

const identUser = "user"

type Env struct {
	cel       *cel.Env
	fields    map[string]schema.Field
	maps      map[string]bool
	scalars   map[string]bool
	userAttrs map[string]bool
}

type Program struct {
	Source string
	root   expression.Expr
}

func NewEnv(m schema.Model) (*Env, error) {
	opts := []cel.EnvOption{
		cel.Variable(identUser, cel.MapType(cel.StringType, cel.StringType)),
		cel.ClearMacros(),
		cel.Function("exists", cel.Overload("exists_dyn", []*cel.Type{cel.DynType}, cel.BoolType)),
		cel.Function("hasAny", cel.MemberOverload("list_hasAny", []*cel.Type{cel.DynType, cel.ListType(cel.DynType)}, cel.BoolType)),
		cel.Function("hasAll", cel.MemberOverload("list_hasAll", []*cel.Type{cel.DynType, cel.ListType(cel.DynType)}, cel.BoolType)),
		cel.Function("between", cel.MemberOverload("number_between", []*cel.Type{cel.DynType, cel.DynType, cel.DynType}, cel.BoolType)),
	}
	e := &Env{fields: map[string]schema.Field{}, maps: map[string]bool{}, scalars: map[string]bool{}, userAttrs: map[string]bool{}}
	for _, f := range m.Fields {
		if !f.Policy {
			continue
		}
		if err := f.Validate(); err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Ref(), err)
		}
		e.fields[f.Key()] = f
		if f.Map == "" {
			if f.Name == identUser {
				return nil, fmt.Errorf("a column named %q cannot be referenced: the identifier is reserved for caller attributes", identUser)
			}
			if !e.scalars[f.Name] {
				e.scalars[f.Name] = true
				opts = append(opts, cel.Variable(f.Name, celType(f.Type())))
			}
			continue
		}
		if f.Map == identUser {
			return nil, fmt.Errorf("a map column named %q cannot be referenced: the identifier is reserved for caller attributes", identUser)
		}
		if e.scalars[f.Map] {
			return nil, fmt.Errorf("%q is marked both as a scalar column and as a map column", f.Map)
		}
		if !e.maps[f.Map] {
			e.maps[f.Map] = true
			opts = append(opts, cel.Variable(f.Map, cel.MapType(cel.StringType, cel.DynType)))
		}
	}
	for _, a := range m.UserAttrs {
		e.userAttrs[a] = true
	}
	var err error
	e.cel, err = cel.NewEnv(opts...)
	return e, err
}

func celType(t schema.LogicalType) *cel.Type {
	switch t {
	case schema.TypeString:
		return cel.StringType
	case schema.TypeStringArray:
		return cel.ListType(cel.StringType)
	case schema.TypeBoolean:
		return cel.BoolType
	default:
		return cel.DynType
	}
}

func (e *Env) Compile(src string) (*Program, error) {
	parsed, issues := e.cel.Compile(src)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("policy %q: %w", src, issues.Err())
	}
	root, err := e.convert(parsed.NativeRep().Expr())
	if err == nil {
		err = expression.Validate(root, expression.PolicyProfile)
	}
	if err != nil {
		return nil, fmt.Errorf("policy %q: %w", src, err)
	}
	return &Program{Source: src, root: root}, nil
}

func (e *Env) convert(n ast.Expr) (expression.Expr, error) {
	switch n.Kind() {
	case ast.LiteralKind:
		if n.AsLiteral() == types.True {
			return expression.Expr{Op: expression.True}, nil
		}
		if n.AsLiteral() == types.False {
			return expression.Expr{Op: expression.False}, nil
		}
		return expression.Expr{}, fmt.Errorf("a bare literal is not a policy")
	case ast.IdentKind:
		return expression.Expr{}, fmt.Errorf("%q is not a policy on its own", n.AsIdent())
	case ast.SelectKind:
		return expression.Expr{}, fmt.Errorf("an attribute key is written MapColumn[\"key\"], not MapColumn.key: attribute names contain dots, and CEL reads a dot as member access")
	case ast.ComprehensionKind:
		return expression.Expr{}, fmt.Errorf("macros are not allowed: they have no SQL translation")
	case ast.CallKind:
		c := n.AsCall()
		switch c.FunctionName() {
		case operators.LogicalAnd, operators.LogicalOr:
			op := expression.And
			if c.FunctionName() == operators.LogicalOr {
				op = expression.Or
			}
			out := expression.Expr{Op: op}
			for _, a := range c.Args() {
				child, err := e.convert(a)
				if err != nil {
					return out, err
				}
				out.Children = append(out.Children, child)
			}
			return out, nil
		case operators.LogicalNot:
			child, err := e.convert(c.Args()[0])
			return expression.Expr{Op: expression.Not, Children: []expression.Expr{child}}, err
		case operators.Equals, operators.NotEquals, operators.Greater, operators.GreaterEquals, operators.Less, operators.LessEquals:
			return e.comparison(c.FunctionName(), c.Args()[0], c.Args()[1])
		case operators.In:
			return e.membership(c.Args()[0], c.Args()[1])
		case "startsWith", "hasAny", "hasAll", "between":
			if !c.IsMemberFunction() {
				return expression.Expr{}, fmt.Errorf("%s must be called on a field", c.FunctionName())
			}
			return e.member(c.FunctionName(), c.Target(), c.Args())
		case "exists":
			if len(c.Args()) != 1 {
				return expression.Expr{}, fmt.Errorf("exists takes one field")
			}
			f, ok, err := e.fieldRef(c.Args()[0])
			if err != nil {
				return expression.Expr{}, err
			}
			if !ok {
				return expression.Expr{}, fmt.Errorf("exists takes a declared field")
			}
			return expression.Expr{Op: expression.Exists, Field: f}, nil
		case operators.Index:
			return expression.Expr{}, fmt.Errorf("a field reference is not a policy on its own; compare it to something")
		}
	}
	return expression.Expr{}, fmt.Errorf("unsupported expression")
}

func (e *Env) comparison(fn string, a, b ast.Expr) (expression.Expr, error) {
	f, ok, ferr := e.fieldRef(a)
	reversed := false
	if ferr != nil && ok {
		return expression.Expr{}, ferr
	}
	if !ok {
		f, ok, ferr = e.fieldRef(b)
		reversed = true
		if ferr != nil && ok {
			return expression.Expr{}, ferr
		}
	}
	if !ok {
		if hint := e.bracketHint(a, b); hint != "" {
			return expression.Expr{}, fmt.Errorf("%s", hint)
		}
		return expression.Expr{}, fmt.Errorf("one side of a comparison must be a declared field: a marked column, or a marked key written MapColumn[\"key\"]")
	}
	valueNode := b
	if reversed {
		valueNode = a
	}
	v, err := e.value(valueNode, f.Type())
	if err != nil {
		return expression.Expr{}, err
	}
	op := map[string]expression.Operator{operators.Equals: expression.Equal, operators.NotEquals: expression.NotEqual, operators.Greater: expression.Greater, operators.GreaterEquals: expression.GreaterEq, operators.Less: expression.Less, operators.LessEquals: expression.LessEq}[fn]
	if reversed {
		switch op {
		case expression.Greater:
			op = expression.Less
		case expression.GreaterEq:
			op = expression.LessEq
		case expression.Less:
			op = expression.Greater
		case expression.LessEq:
			op = expression.GreaterEq
		}
	}
	return expression.Expr{Op: op, Field: f, Value: v}, nil
}

func (e *Env) membership(left, right ast.Expr) (expression.Expr, error) {
	if f, ok, err := e.fieldRef(left); ok {
		if err != nil {
			return expression.Expr{}, err
		}
		if f.Type() != schema.TypeString {
			return expression.Expr{}, fmt.Errorf("a field on the left of in must be a string")
		}
		vs, err := e.list(right, schema.TypeString)
		return expression.Expr{Op: expression.In, Field: f, Values: vs}, err
	}
	f, ok, err := e.fieldRef(right)
	if err != nil {
		return expression.Expr{}, err
	}
	if !ok || f.Type() != schema.TypeStringArray {
		return expression.Expr{}, fmt.Errorf("in must be a string field in a list, or a string value in a string_array field")
	}
	v, err := e.value(left, schema.TypeString)
	return expression.Expr{Op: expression.In, Field: f, Value: v}, err
}

func (e *Env) member(name string, target ast.Expr, args []ast.Expr) (expression.Expr, error) {
	f, ok, err := e.fieldRef(target)
	if err != nil {
		return expression.Expr{}, err
	}
	if !ok {
		return expression.Expr{}, fmt.Errorf("%s must be called on a declared field", name)
	}
	op := map[string]expression.Operator{"startsWith": expression.StartsWith, "hasAny": expression.HasAny, "hasAll": expression.HasAll, "between": expression.Between}[name]
	out := expression.Expr{Op: op, Field: f}
	if name == "startsWith" {
		if len(args) != 1 {
			return out, fmt.Errorf("startsWith takes one value")
		}
		out.Value, err = e.value(args[0], schema.TypeString)
		return out, err
	}
	if name == "between" {
		if len(args) != 2 {
			return out, fmt.Errorf("between takes two values")
		}
		for _, a := range args {
			v, xerr := e.value(a, schema.TypeNumber)
			if xerr != nil {
				return out, xerr
			}
			out.Values = append(out.Values, v)
		}
		return out, nil
	}
	if len(args) != 1 {
		return out, fmt.Errorf("%s takes one list", name)
	}
	out.Values, err = e.list(args[0], schema.TypeString)
	return out, err
}

func (e *Env) value(n ast.Expr, want schema.LogicalType) (expression.Value, error) {
	if n.Kind() == ast.SelectKind {
		sel := n.AsSelect()
		if sel.Operand().Kind() == ast.IdentKind && sel.Operand().AsIdent() == identUser {
			if !e.userAttrs[sel.FieldName()] {
				return expression.Value{}, fmt.Errorf("user.%s is not in user_attributes", sel.FieldName())
			}
			if want != schema.TypeString {
				return expression.Value{}, fmt.Errorf("caller attributes can only be used as string values")
			}
			return expression.Value{Attr: sel.FieldName()}, nil
		}
	}
	if n.Kind() != ast.LiteralKind {
		return expression.Value{}, fmt.Errorf("a comparison value must be a string literal or user.<attribute>")
	}
	raw := n.AsLiteral().Value()
	var v any
	switch x := raw.(type) {
	case string:
		v = x
	case bool:
		v = x
	case int64:
		v = x
	case uint64:
		v = x
	case float64:
		v = x
	default:
		return expression.Value{}, fmt.Errorf("unsupported literal value")
	}
	out := expression.Value{Literal: v}
	probe := expression.Expr{Op: expression.Equal, Field: schema.Field{Name: "value", LogicalType: want}, Value: out}
	if err := expression.Validate(probe, expression.FilterProfile); err != nil {
		if want == schema.TypeString {
			return out, fmt.Errorf("no matching overload: value for string must be a string")
		}
		return out, fmt.Errorf("value for %s must be a %s", want, want)
	}
	return out, nil
}

func (e *Env) list(n ast.Expr, want schema.LogicalType) ([]expression.Value, error) {
	if n.Kind() != ast.ListKind {
		return nil, fmt.Errorf("expected a list")
	}
	var out []expression.Value
	for _, el := range n.AsList().Elements() {
		v, err := e.value(el, want)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (e *Env) fieldRef(n ast.Expr) (schema.Field, bool, error) {
	if n.Kind() == ast.IdentKind {
		name := n.AsIdent()
		if e.maps[name] {
			return schema.Field{}, true, fmt.Errorf("%s is a map column: index into it, as %s[\"key\"]", name, name)
		}
		if !e.scalars[name] {
			return schema.Field{}, false, nil
		}
		return e.fields[schema.Field{Name: name}.Key()], true, nil
	}
	if n.Kind() == ast.CallKind {
		c := n.AsCall()
		if c.FunctionName() != operators.Index || c.Args()[0].Kind() != ast.IdentKind {
			return schema.Field{}, false, nil
		}
		col := c.Args()[0].AsIdent()
		if !e.maps[col] {
			return schema.Field{}, false, nil
		}
		idx := c.Args()[1]
		if idx.Kind() != ast.LiteralKind {
			return schema.Field{}, true, fmt.Errorf("an attribute key must be a literal string")
		}
		key, ok := idx.AsLiteral().Value().(string)
		if !ok {
			return schema.Field{}, true, fmt.Errorf("an attribute key must be a string")
		}
		f, ok := e.fields[schema.Field{Map: col, Name: key}.Key()]
		if !ok {
			return schema.Field{}, true, fmt.Errorf("%s[%q] is not a field a policy may reference", col, key)
		}
		return f, true, nil
	}
	return schema.Field{}, false, nil
}

func (e *Env) bracketHint(exprs ...ast.Expr) string {
	for _, n := range exprs {
		if n.Kind() == ast.SelectKind {
			s := n.AsSelect()
			if s.Operand().Kind() == ast.IdentKind && e.maps[s.Operand().AsIdent()] {
				return fmt.Sprintf("write %s[\"key\"] rather than %s.%s: attribute names contain dots, and CEL reads a dot as member access", s.Operand().AsIdent(), s.Operand().AsIdent(), s.FieldName())
			}
		}
	}
	return ""
}

type Registry map[string][]*Program

func BuildRegistry(e *Env, roles []schema.Role) (Registry, error) {
	reg := Registry{}
	var errs []string
	for _, r := range roles {
		for _, p := range r.Policies {
			prog, err := e.Compile(p.Expression)
			if err != nil {
				errs = append(errs, fmt.Sprintf("role %q, policy %q: %v", r.Name, p.Title, err))
				continue
			}
			reg[r.Name] = append(reg[r.Name], prog)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid policies:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return reg, nil
}

func (e *Env) AdminProgram() (*Program, error) { return e.Compile("true") }
