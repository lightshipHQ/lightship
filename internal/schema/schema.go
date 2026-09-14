// Package schema is the model of the customer's ClickHouse table: which columns play which
// structural role, and which columns and attribute keys lightship is allowed to reference.
//
// Nothing here is assumed. The layout is discovered from the live database and marked by an
// operator, because a proxy that hardcodes a table shape can only ever front the deployments that
// happen to match it. The binding is the exception and cannot be discovered away: its roles are
// assertions about what a span *is* — grouped into a trace, ordered in time, identified, linked to
// a parent, named — not about how an exporter happens to spell it. The guarantee is
// `GROUP BY <trace_id> HAVING countIf(policy) > 0`, so without knowing which column groups spans
// into traces there is no quantifier and no product.
package schema

import (
	"fmt"
	"strings"
)

// Binding maps a structural role to a column in the customer's table. Every role is required: each
// is something the OTel span data model guarantees a span table carries, so a table missing one is
// not a span table. Notably absent: service, which is an ordinary markable field like tenant.id,
// and duration, which is computed from the timestamps rather than bound.
type Binding struct {
	Table        string `json:"table"`
	TraceID      string `json:"trace_id"`
	Timestamp    string `json:"timestamp"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id"`
	Name         string `json:"name"`
}

func (b Binding) Validate() error {
	var errs []string
	if b.Table == "" {
		errs = append(errs, "table is required")
	} else if !ValidTable(b.Table) {
		errs = append(errs, fmt.Sprintf("table must be database.table using plain identifiers, got %q", b.Table))
	}
	if b.TraceID == "" {
		errs = append(errs, "trace_id is required: it is the column spans are grouped by, and the "+
			"authorization quantifier has no meaning without it")
	}
	if b.Timestamp == "" {
		errs = append(errs, "timestamp is required: the query window, the cursor and the trace "+
			"duration are built on it")
	}
	if b.SpanID == "" {
		errs = append(errs, "span_id is required: it is what identifies a span, and what a child "+
			"span's parent reference points at")
	}
	if b.ParentSpanID == "" {
		errs = append(errs, "parent_span_id is required: it is how the root span is told apart "+
			"from the rest, and how a trace becomes a tree")
	}
	if b.Name == "" {
		errs = append(errs, "name is required: it is what labels a span, and the trace list's "+
			"root operation")
	}
	for role, col := range map[string]string{
		"trace_id": b.TraceID, "timestamp": b.Timestamp, "span_id": b.SpanID,
		"parent_span_id": b.ParentSpanID, "name": b.Name,
	} {
		if col != "" && !ValidIdent(col) {
			errs = append(errs, fmt.Sprintf("%s: %q is not a usable column name", role, col))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid source binding:\n  - %s\nRe-run setup, or PUT /schema/binding "+
			"with a column for every role.", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Field is one thing a policy or a filter may reference: either a scalar column (Map empty), or a
// key inside a map column.
//
// The two flags are independent and mean different things. Filterable lets a caller narrow their
// own query, which can only ever shrink what they already see. Policy admits the field to the
// vocabulary of the security model — not a grant in itself, but the set a policy may be written
// against. The expensive mistake with either is omission: a field left unmarked is a query nobody
// can express, or a restriction nobody can write.
type Field struct {
	Map         string      `json:"map"`  // "" when this is a scalar column
	Name        string      `json:"name"` // the column name, or the key within Map
	LogicalType LogicalType `json:"logical_type,omitempty"`
	Filterable  bool        `json:"filterable"`
	Policy      bool        `json:"policy"`
}

// LogicalType is the application-level type of a marked field. An empty value reads as string so
// models written before typed fields were introduced keep their meaning.
type LogicalType string

const (
	TypeString      LogicalType = "string"
	TypeStringArray LogicalType = "string_array"
	TypeBoolean     LogicalType = "boolean"
	TypeNumber      LogicalType = "number"
)

func (f Field) Type() LogicalType {
	if f.LogicalType == "" {
		return TypeString
	}
	return f.LogicalType
}

// Ref is how a field is written in a policy and named in the API: `ServiceName`, or
// `ResourceAttributes["tenant.id"]`.
func (f Field) Ref() string {
	if f.Map == "" {
		return f.Name
	}
	return fmt.Sprintf("%s[%q]", f.Map, f.Name)
}

// Key identifies a field uniquely.
func (f Field) Key() string { return f.Map + "\x00" + f.Name }

func (f Field) Validate() error {
	if f.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch f.Type() {
	case TypeString, TypeStringArray, TypeBoolean, TypeNumber:
	default:
		return fmt.Errorf("unknown logical_type %q: expected string, string_array, boolean, or number", f.LogicalType)
	}
	if f.Policy && f.Type() == TypeNumber {
		return fmt.Errorf("number fields cannot be policy-referenceable")
	}
	if f.Map == "" {
		// A scalar column is a bare CEL identifier, so its name has to be one. A ClickHouse column
		// may contain a dot (`Events.Timestamp`); CEL reads a dot as member access, so such a
		// column cannot be referenced and is rejected rather than silently mis-parsed.
		if !ValidIdent(f.Name) {
			return fmt.Errorf("column %q cannot be referenced: a policy names columns directly, so "+
				"the name must be a plain identifier. Names containing a dot are read by CEL as "+
				"member access", f.Name)
		}
		return nil
	}
	if !ValidIdent(f.Map) {
		return fmt.Errorf("map column %q is not a usable identifier", f.Map)
	}
	// The key is only ever a quoted string, in CEL and in the filter DSL alike, so dots in an
	// attribute key are fine and always have been. That is what bracket notation is for.
	return nil
}

// ValidIdent reports whether a name can be used as a bare identifier in a policy and interpolated
// into SQL. Only column names and attribute keys ever reach the SQL text, so this is the boundary
// that keeps them safe.
func ValidIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ValidTable accepts exactly the unquoted database.table shape used in generated ClickHouse SQL.
func ValidTable(s string) bool {
	parts := strings.Split(s, ".")
	return len(parts) == 2 && ValidIdent(parts[0]) && ValidIdent(parts[1])
}

// Policy is one CEL expression on a role. Title is for error messages and for the audit trail.
type Policy struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Expression  string `json:"expression"`
}

// Role carries one or more policies. Policies OR together across a caller's roles, so the model is
// monotonic: holding another role can only widen what a caller sees.
type Role struct {
	Name     string   `json:"name"`
	Policies []Policy `json:"policies"`
}

// Model is the whole access model, read as one coherent snapshot so that a request is never served
// by a mix of versions. Version increments on every write; replicas hold a compiled snapshot with a
// short TTL rather than reading Postgres per request, and the version is how a reload announces it
// picked up a change.
type Model struct {
	Binding   Binding  `json:"binding"`
	Fields    []Field  `json:"fields"`
	UserAttrs []string `json:"user_attributes"`
	Roles     []Role   `json:"roles"`
	Version   int64    `json:"version"`
}

// Filterable is the subset a caller may use to narrow a trace query.
func (m Model) Filterable() []Field {
	out := []Field{}
	for _, f := range m.Fields {
		if f.Filterable {
			out = append(out, f)
		}
	}
	return out
}

// MapColumns returns the distinct map columns any marked field lives in. These become the CEL
// variables a policy indexes into.
func (m Model) MapColumns() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range m.Fields {
		if f.Map != "" && !seen[f.Map] {
			seen[f.Map] = true
			out = append(out, f.Map)
		}
	}
	return out
}
