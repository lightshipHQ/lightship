package traces

import (
	"context"
	"fmt"
	"strings"

	"github.com/lightshipHQ/lightship/internal/schema"
)

// Discovery reads the customer's ClickHouse to find out what is actually there. It lives in this
// package for the same reason every other query does: the driver is imported once, so there is one
// place where SQL is built and one place to review.
//
// Nothing here decides anything. It reports what exists and how well each candidate matches the
// shape a trace table usually has; an operator marks what lightship may reference. A proxy that
// guessed would be a proxy that only fronts the deployments that happen to match its guess.

// Column is one column of a candidate table.
type Column struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Indexed bool   `json:"indexed"` // in the primary key, or carrying a data-skipping index
}

// TableCandidate is a table that might hold spans, with a score for how closely it matches the
// usual shape. The score orders the list; it does not choose.
type TableCandidate struct {
	Database string   `json:"database"`
	Table    string   `json:"table"`
	Score    int      `json:"score"`
	Columns  []Column `json:"columns"`
	// Suggested is a best-effort role binding by column name, offered as a starting point.
	Suggested schema.Binding `json:"suggested_binding"`
	// SuggestedFields are non-structural columns worth marking, prechecked on the field-marking
	// screen. Service is the one case today: it left the binding because it is an ordinary field,
	// and this keeps the promoted-column layout from being worse off for that.
	SuggestedFields []schema.Field `json:"suggested_fields,omitempty"`
}

// roleHints are the column names each structural role usually has. They only order the list and
// prefill a suggestion; a table matching none of them is still listed, with an empty suggestion for
// the operator to fill in.
var roleHints = map[string][]string{
	"trace_id":       {"TraceId", "trace_id", "TraceID"},
	"timestamp":      {"Timestamp", "timestamp", "StartTime", "start_time"},
	"span_id":        {"SpanId", "span_id", "SpanID"},
	"parent_span_id": {"ParentSpanId", "parent_span_id", "ParentSpanID"},
	"name":           {"SpanName", "span_name", "Name", "name"},
}

// serviceHints are the names a promoted service column usually has. Service is not a role, so a
// match feeds SuggestedFields rather than the binding.
var serviceHints = []string{"ServiceName", "service_name"}

// DiscoverTables lists tables that could hold spans, best match first.
func (r *Reader) DiscoverTables(ctx context.Context) ([]TableCandidate, error) {
	rows, err := r.conn.Query(ctx, `
		SELECT database, table, name, type
		FROM system.columns
		WHERE database NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema')
		ORDER BY database, table, position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byTable := map[string]*TableCandidate{}
	var order []string
	for rows.Next() {
		var db, tbl, name, typ string
		if err := rows.Scan(&db, &tbl, &name, &typ); err != nil {
			return nil, err
		}
		k := db + "." + tbl
		c, ok := byTable[k]
		if !ok {
			c = &TableCandidate{Database: db, Table: tbl}
			byTable[k] = c
			order = append(order, k)
		}
		c.Columns = append(c.Columns, Column{Name: name, Type: typ})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	indexed, err := r.indexedColumns(ctx)
	if err != nil {
		return nil, err
	}

	out := []TableCandidate{}
	for _, k := range order {
		c := byTable[k]
		has := map[string]bool{}
		for i := range c.Columns {
			has[c.Columns[i].Name] = true
			c.Columns[i].Indexed = indexed[k+"."+c.Columns[i].Name]
		}
		c.Suggested.Table = k
		for role, names := range roleHints {
			for _, n := range names {
				if has[n] {
					assignRole(&c.Suggested, role, n)
					break
				}
			}
		}
		for _, n := range serviceHints {
			if has[n] {
				c.SuggestedFields = append(c.SuggestedFields,
					schema.Field{Name: n, Filterable: true})
				break
			}
		}
		// Every table is a candidate. The score orders the list and the suggestion is a starting
		// point; neither excludes anything, because a table whose columns are named nothing like
		// the usual ones is exactly the case this whole flow exists to serve. Dropping it would be
		// the naming assumption wearing a helpful face.
		c.Score = score(c.Suggested)
		out = append(out, *c)
	}
	sortByScore(out)
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out, nil
}

// maxCandidates bounds the listing on a database with many tables. The list is ordered by how
// closely each matches the usual shape, so a truncated answer keeps the plausible ones.
const maxCandidates = 50

func assignRole(b *schema.Binding, role, col string) {
	switch role {
	case "trace_id":
		b.TraceID = col
	case "timestamp":
		b.Timestamp = col
	case "span_id":
		b.SpanID = col
	case "parent_span_id":
		b.ParentSpanID = col
	case "name":
		b.Name = col
	}
}

func score(b schema.Binding) int {
	n := 0
	for _, c := range []string{b.TraceID, b.Timestamp, b.SpanID, b.ParentSpanID, b.Name} {
		if c != "" {
			n++
		}
	}
	return n
}

func sortByScore(cs []TableCandidate) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].Score > cs[j-1].Score; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

// indexedColumns reports which columns are in a table's sorting key or carry a data-skipping index.
// It is approximate on purpose: the answer feeds a warning, not a decision.
func (r *Reader) indexedColumns(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := r.conn.Query(ctx,
		`SELECT database, table, sorting_key, primary_key FROM system.tables WHERE sorting_key != ''`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var db, tbl, sortKey, pk string
		if err := rows.Scan(&db, &tbl, &sortKey, &pk); err != nil {
			rows.Close()
			return nil, err
		}
		for _, c := range splitIdents(sortKey + ", " + pk) {
			out[db+"."+tbl+"."+c] = true
		}
	}
	rows.Close()

	rows, err = r.conn.Query(ctx,
		`SELECT database, table, expr FROM system.data_skipping_indices`)
	if err != nil {
		// Older servers may not expose the table; an unknown index is a missing warning, not a
		// failed discovery.
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var db, tbl, expr string
		if err := rows.Scan(&db, &tbl, &expr); err != nil {
			return out, nil
		}
		for _, c := range splitIdents(expr) {
			out[db+"."+tbl+"."+c] = true
		}
	}
	return out, nil
}

// splitIdents pulls bare identifiers out of a sorting key or index expression. Crude by design:
// `toDateTime(Timestamp)` should mark Timestamp, and over-reporting an index costs a warning that
// was not shown rather than a wrong query.
func splitIdents(s string) []string {
	var out []string
	cur := make([]rune, 0, 32)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// AttrKey is one attribute key found in a map column, with the evidence an operator needs to decide
// whether to mark it.
type AttrKey struct {
	Map string `json:"map"`
	Key string `json:"key"`
	// Spans and Coverage report how often a key appears in the sampled spans. Missing it on other
	// spans does not hide a trace with a policy-matching span.
	Spans          uint64  `json:"spans"`
	Coverage       float64 `json:"coverage"`
	DistinctValues uint64  `json:"distinct_values"`
}

// DiscoverKeys samples a window of the table for the keys present in each map column. Keys are
// data, not schema — `system.columns` cannot report them — so this is the only way to find them,
// and it is best-effort by nature: a key absent from the window is not absent from the table. An
// operator must always be able to name one by hand.
func (r *Reader) DiscoverKeys(ctx context.Context, b schema.Binding, maps []string,
	window Window) ([]AttrKey, error) {
	out := []AttrKey{}
	for _, m := range maps {
		if !schema.ValidIdent(m) {
			return nil, fmt.Errorf("%q is not a usable map column name", m)
		}
		var total uint64
		if err := r.conn.QueryRow(ctx, fmt.Sprintf(
			`SELECT count() FROM %s WHERE %s >= ? AND %s < ?`,
			b.Table, b.Timestamp, b.Timestamp), window.From, window.To).Scan(&total); err != nil {
			return nil, err
		}
		rows, err := r.conn.Query(ctx, fmt.Sprintf(`
			SELECT k, count() AS spans, uniqExact(%s[k]) AS vals
			FROM %s ARRAY JOIN mapKeys(%s) AS k
			WHERE %s >= ? AND %s < ?
			GROUP BY k ORDER BY k`, m, b.Table, m, b.Timestamp, b.Timestamp),
			window.From, window.To)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			a := AttrKey{Map: m}
			if err := rows.Scan(&a.Key, &a.Spans, &a.DistinctValues); err != nil {
				rows.Close()
				return nil, err
			}
			if total > 0 {
				a.Coverage = float64(a.Spans) / float64(total)
			}
			out = append(out, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Index is one data-skipping index on the customer's table.
type Index struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Expr string `json:"expression"`
}

// TableInfo is what an optimisation check needs to know about a table: how it is sorted, and what
// skip indexes it already carries.
type TableInfo struct {
	Table      string   `json:"table"`
	SortingKey string   `json:"sorting_key"`
	Columns    []Column `json:"columns"`
	Indexes    []Index  `json:"indexes"`
}

// Has reports whether the table has a column of this name.
func (t TableInfo) Has(col string) bool {
	for _, c := range t.Columns {
		if c.Name == col {
			return true
		}
	}
	return false
}

// Describe reads a table's sorting key and skip indexes. Read-only, like everything here: lightship
// reports what would make a query faster and never alters the customer's schema.
func (r *Reader) Describe(ctx context.Context, table string) (TableInfo, error) {
	db, name, ok := splitTable(table)
	if !ok {
		return TableInfo{}, fmt.Errorf("table must be database.table, got %q", table)
	}
	info := TableInfo{Table: table, Columns: []Column{}, Indexes: []Index{}}
	if err := r.conn.QueryRow(ctx,
		`SELECT sorting_key FROM system.tables WHERE database = ? AND name = ?`,
		db, name).Scan(&info.SortingKey); err != nil {
		return TableInfo{}, err
	}
	cols, err := r.conn.Query(ctx,
		`SELECT name, type FROM system.columns WHERE database = ? AND table = ? ORDER BY position`,
		db, name)
	if err != nil {
		return TableInfo{}, err
	}
	for cols.Next() {
		var c Column
		if err := cols.Scan(&c.Name, &c.Type); err != nil {
			cols.Close()
			return TableInfo{}, err
		}
		info.Columns = append(info.Columns, c)
	}
	cols.Close()
	if len(info.Columns) == 0 {
		return TableInfo{}, fmt.Errorf("table %s does not exist, or has no columns", table)
	}

	rows, err := r.conn.Query(ctx,
		`SELECT name, type_full, expr FROM system.data_skipping_indices
		  WHERE database = ? AND table = ? ORDER BY name`, db, name)
	if err != nil {
		// A server that does not expose the table simply reports no indexes; an unknown index costs
		// a suggestion that was already applied, not a wrong answer.
		return info, nil
	}
	defer rows.Close()
	for rows.Next() {
		var i Index
		if err := rows.Scan(&i.Name, &i.Type, &i.Expr); err != nil {
			return info, nil
		}
		info.Indexes = append(info.Indexes, i)
	}
	return info, rows.Err()
}

func splitTable(t string) (db, name string, ok bool) {
	i := strings.IndexByte(t, '.')
	if i <= 0 || i == len(t)-1 {
		return "", "", false
	}
	return t[:i], t[i+1:], true
}
