// Package traces is the only place a ClickHouse query is built or executed.
//
// That is enforced structurally rather than by discipline — see chokepoint_test.go, which fails
// the build if the driver is imported anywhere else. The reason is narrow: a policy predicate that
// is appended in one place can be reviewed in one place, and a second query path is how a filter
// gets forgotten.
//
// Every statement here selects from a trace-id set produced by policy.Selection. Client-supplied
// filters narrow that set further; they are ANDed after the policy and can never widen it.
package traces

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/lightshipHQ/lightship/internal/policy"
	"github.com/lightshipHQ/lightship/internal/schema"
)

var ErrNotVisible = errors.New("trace not found")

type Reader struct{ conn clickhouse.Conn }

func New(conn clickhouse.Conn) *Reader { return &Reader{conn: conn} }

// Open builds the connection here rather than in main, so that the driver is imported by exactly
// one package and the chokepoint rule has no exception to argue about.
//
// It does not fail when the source is unreachable. ClickHouse is not a boot dependency: an
// unreachable source must deny queries, not stop the control plane from starting, because the
// audit log and the health endpoint have to keep working while it is down.
func Open(dsn string) (*Reader, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("source.dsn: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("source.dsn: %w", err)
	}
	return New(conn), nil
}

func (r *Reader) Close() error { return r.conn.Close() }

// Caller carries everything the policy needs about who is asking.
type Caller struct {
	Username string
	Attrs    map[string]string
	Programs []*policy.Program
}

// Query is one request's whole context: which table and columns to read, the compiled vocabulary
// to translate against, and who is asking. The first two used to be fixed at startup; they are the
// operator's discovered schema now, so they arrive with the request and can change under it.
type Query struct {
	Binding schema.Binding
	Env     *policy.Env
	// Model is the snapshot the binding and the vocabulary came from. A filter is built against it
	// rather than against a freshly read one, so a request cannot mix a policy compiled under one
	// version with a filter validated under another.
	Model  schema.Model
	Caller Caller
}

type Window struct {
	From, To time.Time
}

// Cursor is the position of the last row a client received. Pagination is forward-only by design:
// a client scrolls chronologically and cannot jump, so there is no page number to keep stable as
// new traces arrive.
type Cursor struct {
	// StartNanos rather than a time.Time: the driver marshals a DateTime64(9) parameter down to
	// second precision, which silently drops every trace sharing the cursor's second at a page
	// boundary. An integer comparison has no such conversion in it.
	StartNanos int64
	StartTime  time.Time
	TraceID    string
}

type TraceSummary struct {
	TraceID   string    `json:"trace_id"`
	StartTime time.Time `json:"start_time"`
	Duration  float64   `json:"duration_ms"`
	RootName  string    `json:"root_name"`
	SpanCount uint64    `json:"span_count"`

	startNanos int64 // cursor position; not part of the response
}

type ListResult struct {
	Traces []TraceSummary `json:"traces"`
	Next   *Cursor        `json:"-"`
}

func (r *Reader) List(ctx context.Context, q Query, w Window, f *policy.Query, cur *Cursor,
	limit int) (ListResult, error) {
	b := q.Binding
	pred, err := q.Env.Predicate(q.Caller.Programs, q.Caller.Attrs)
	if err != nil {
		// No policy can apply. Deny without touching ClickHouse — no result would be correct.
		return ListResult{Traces: []TraceSummary{}}, nil
	}
	window := policy.Query{
		SQL:  fmt.Sprintf("%s >= ? AND %s < ?", b.Timestamp, b.Timestamp),
		Args: []any{w.From, w.To},
	}
	// The policy narrows by trace id through an indexable subquery; the caller's filter and the
	// cursor are group-level and stay in the HAVING. See policy.TraceIDs for why this split.
	ids := policy.TraceIDs(b, window, pred)

	var having []string
	var havingArgs []any
	if f != nil && f.SQL != "" {
		h := policy.Having(*f)
		having = append(having, h.SQL)
		havingArgs = append(havingArgs, h.Args...)
	}
	if cur != nil {
		// Tuple comparison keeps ties on StartTime ordered deterministically by trace id, so a
		// scroll cannot repeat or skip a trace that shares a timestamp with its neighbour.
		having = append(having, "(StartNanos, TraceKey) < (?, ?)")
		havingArgs = append(havingArgs, cur.StartNanos, cur.TraceID)
	}
	havingSQL := ""
	if len(having) > 0 {
		havingSQL = "\n\t\tHAVING " + strings.Join(having, " AND ")
	}

	// Trace duration is wall clock across the trace's spans, which every table can answer from
	// its timestamps alone. A span-level duration column, where one exists, is in the table's own
	// units and is the caller's to project like any other column.
	duration := fmt.Sprintf(
		"(toUnixTimestamp64Nano(max(%s)) - toUnixTimestamp64Nano(min(%s))) / 1e6",
		b.Timestamp, b.Timestamp)
	rootName := fmt.Sprintf("argMin(%s, %s)", b.Name, b.Timestamp)
	sql := fmt.Sprintf(`SELECT %s AS TraceKey,
			min(%s) AS StartTime,
			toUnixTimestamp64Nano(min(%s)) AS StartNanos,
			%s AS DurationMs,
			%s AS RootName,
			count() AS SpanCount
		FROM %s
		WHERE %s AND %s IN (%s)
		GROUP BY TraceKey%s
		ORDER BY StartNanos DESC, TraceKey DESC
		LIMIT ?`,
		b.TraceID, b.Timestamp, b.Timestamp, duration, rootName,
		b.Table, window.SQL, b.TraceID, ids.SQL, havingSQL)

	args := append([]any{}, window.Args...)
	args = append(args, ids.Args...)
	args = append(args, havingArgs...)
	args = append(args, limit)

	rows, err := r.conn.Query(ctx, sql, args...)
	if err != nil {
		return ListResult{}, err
	}
	defer rows.Close()

	out := ListResult{Traces: []TraceSummary{}}
	for rows.Next() {
		var t TraceSummary
		var startNanos int64
		if err := rows.Scan(&t.TraceID, &t.StartTime, &startNanos, &t.Duration, &t.RootName,
			&t.SpanCount); err != nil {
			return ListResult{}, err
		}
		t.startNanos = startNanos
		out.Traces = append(out.Traces, t)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, err
	}
	if n := len(out.Traces); n > 0 {
		last := out.Traces[n-1]
		out.Next = &Cursor{StartNanos: last.startNanos, StartTime: last.StartTime, TraceID: last.TraceID}
	}

	return out, nil
}

// Projection is the columns a read should return, empty meaning every column of the row.
//
// Names are validated as identifiers before they reach the SQL text — only column names and
// attribute keys ever do — and a name the table does not have surfaces as an error from ClickHouse
// rather than being silently dropped. A projection is about response size, not authorization: it
// may name any column, marked or not, because what a caller may see was already decided by the
// policy over whole traces.
type Projection []string

// lists renders the inner and outer SELECT lists for a query that wraps rows in a subquery.
//
// The two differ because the query needs columns the caller did not ask for: the trace id to
// partition by and the timestamp to order by. Those go in the inner list and not the outer, so a
// projection that leaves them out still produces a valid query and a response containing only what
// was requested. Without this a `columns=SpanId` read fails with "unknown identifier Timestamp",
// which is a confusing way to learn that ordering needs a column.
// Validate reports a column name that could not be a column. The handler calls it so a client's
// mistake is a 400 rather than a ClickHouse failure relayed as a 502 — the reader checks again, but
// by then the status has already been decided.
func (p Projection) Validate() error {
	for _, c := range p {
		if !schema.ValidIdent(c) {
			return fmt.Errorf("%q is not a usable column name", c)
		}
	}
	return nil
}

func (p Projection) lists(needed ...string) (inner, outer string, err error) {
	if len(p) == 0 {
		return "*", "* EXCEPT (" + permittedCol + ")", nil
	}
	seen := map[string]bool{}
	cols := make([]string, 0, len(p)+len(needed))
	for _, c := range p {
		if !schema.ValidIdent(c) {
			return "", "", fmt.Errorf("%q is not a usable column name", c)
		}
		if !seen[c] {
			seen[c] = true
			cols = append(cols, c)
		}
	}
	outer = strings.Join(cols, ", ")
	for _, c := range needed {
		if c != "" && !seen[c] {
			seen[c] = true
			cols = append(cols, c)
		}
	}
	return strings.Join(cols, ", "), outer, nil
}

// permittedCol is the window-function marker the detail read uses. Prefixed so it cannot collide
// with a column in the customer's table.
const permittedCol = "__lightship_permitted"

// Spans returns the rows of every trace on one page of the same scroll GET /traces walks.
//
// This is what an agent fetches to a file. It is a page rather than an export because pagination is
// the export: successive pages give everything, through one query path with one place the policy is
// applied, instead of a second endpoint that would have to re-derive it.
//
// limit counts traces, not spans — the policy is a trace-level predicate and the cursor is a trace
// position, so a page is N traces and however many spans they hold.
func (r *Reader) Spans(ctx context.Context, q Query, w Window, f *policy.Query, cur *Cursor,
	limit int, p Projection) (*SpanRows, error) {
	b := q.Binding
	// The page of trace ids, and the cursor, come from the same aggregate the summary list uses, so
	// a scroll over rows and a scroll over summaries visit traces in exactly the same order.
	page, err := r.List(ctx, q, w, f, cur, limit)
	if err != nil {
		return nil, err
	}
	if len(page.Traces) == 0 {
		return &SpanRows{}, nil
	}
	ids := make([]any, 0, len(page.Traces))
	holders := make([]string, 0, len(page.Traces))
	for _, t := range page.Traces {
		ids = append(ids, t.TraceID)
		holders = append(holders, "?")
	}

	// The policy is re-applied here rather than inherited from the page above.
	//
	// The ids already came from an authorized query, so this changes no result — it changes what
	// the guarantee rests on. Without it, safety would be the call-site convention "these ids came
	// from List", which the next person to touch this function has no way to see. With it, the
	// statement is self-authorizing: hand it any trace id at all and it returns nothing unless a
	// span of that trace satisfies the caller's policy, exactly as the single-trace read does.
	//
	// It costs no extra scan. These rows are being read regardless, and a second window aggregate
	// over the same partition is evaluated alongside the one that orders them.
	pred, err := q.Env.Predicate(q.Caller.Programs, q.Caller.Attrs)
	if err != nil {
		return &SpanRows{}, nil
	}

	// Ordered so the file reads the way the page does: traces newest first, spans in time order
	// within each. The window function supplies the trace's start without a second aggregation.
	const ordCol = "__lightship_trace_start"
	inner, outer, err := p.lists(b.TraceID, b.Timestamp)
	if err != nil {
		return nil, err
	}
	if len(p) == 0 {
		outer = fmt.Sprintf("* EXCEPT (%s, %s)", ordCol, permittedCol)
	}
	sql := fmt.Sprintf(`SELECT %s FROM (
			SELECT %s,
			       min(%s) OVER (PARTITION BY %s) AS %s,
			       sum(%s) OVER (PARTITION BY %s) AS %s
			FROM %s WHERE %s IN (%s)
		) WHERE %s > 0 ORDER BY %s DESC, %s DESC, %s ASC`,
		outer, inner,
		b.Timestamp, b.TraceID, ordCol,
		pred.SQL, b.TraceID, permittedCol,
		b.Table, b.TraceID, strings.Join(holders, ", "),
		permittedCol, ordCol, b.TraceID, b.Timestamp)

	args := append([]any{}, pred.Args...)
	args = append(args, ids...)
	rows, err := r.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	next := page.Next
	if len(page.Traces) < limit {
		next = nil
	}
	return newSpanRows(rows, next), nil
}

// Span is one row of the customer's table, as it is stored. Keys are their column names: there is
// no fixed idea here of what a span looks like, so renaming or subsetting would be an opinion.
type Span map[string]any

// SpanRows exposes one decoded ClickHouse row at a time. Callers must close it. Keeping the driver
// cursor open lets an HTTP response apply backpressure to ClickHouse instead of collecting every
// span in a trace page in the control-plane heap.
type SpanRows struct {
	rows  driver.Rows
	names []string
	types []driver.ColumnType
	next  *Cursor
}

func newSpanRows(rows driver.Rows, next *Cursor) *SpanRows {
	return &SpanRows{rows: rows, names: rows.Columns(), types: rows.ColumnTypes(), next: next}
}

// Read advances and decodes one span. ok is false only at a clean end of stream.
func (s *SpanRows) Read() (span Span, ok bool, err error) {
	if s.rows == nil {
		return nil, false, nil
	}
	if !s.rows.Next() {
		if err := s.rows.Err(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	dest := make([]any, len(s.names))
	for i := range dest {
		dest[i] = reflect.New(s.types[i].ScanType()).Interface()
	}
	if err := s.rows.Scan(dest...); err != nil {
		return nil, false, err
	}
	span = Span{}
	for i, name := range s.names {
		span[name] = reflect.ValueOf(dest[i]).Elem().Interface()
	}
	return span, true, nil
}

func (s *SpanRows) Close() error {
	if s.rows == nil {
		return nil
	}
	return s.rows.Close()
}

func (s *SpanRows) NextCursor() *Cursor { return s.next }

// Detail returns every span of one trace, and ErrNotVisible if the caller may not see it.
//
// Every span and every column, because the unit of access is the trace: a trace with a span satisfying
// the policy is returned whole. Filtering spans within a visible trace would produce waterfalls
// with holes and would drop precisely the shared-infrastructure spans — gateways, retries, cache
// lookups — that carry no marker and are the ones most worth seeing.
func (r *Reader) Detail(ctx context.Context, q Query, traceID string, p Projection) ([]Span, error) {
	b := q.Binding
	inner, outer, err := p.lists(b.TraceID, b.Timestamp)
	if err != nil {
		return nil, err
	}
	pred, err := q.Env.Predicate(q.Caller.Programs, q.Caller.Attrs)
	if err != nil {
		return nil, ErrNotVisible
	}

	// One read of the trace's rows. The window function turns the per-span predicate into the
	// trace-level answer without referring to the table a second time, and the outer filter drops
	// every row when no span qualified — so a trace the caller may not see returns nothing, and one
	// they may see returns all of it.
	//
	// EXCEPT keeps the marker out of the response: the caller gets their columns, not ours.
	sql := fmt.Sprintf(`SELECT %s FROM (
			SELECT %s, sum(%s) OVER (PARTITION BY %s) AS %s
			FROM %s WHERE %s = ?
		) WHERE %s > 0 ORDER BY %s ASC`,
		outer, inner, pred.SQL, b.TraceID, permittedCol,
		b.Table, b.TraceID, permittedCol, b.Timestamp)
	args := append([]any{}, pred.Args...)
	args = append(args, traceID)

	rows, err := r.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		// A trace the caller may not see and a trace that does not exist are the same response:
		// otherwise the error distinguishes them and becomes an existence oracle.
		return nil, ErrNotVisible
	}
	return out, nil
}

func (r *Reader) Ping(ctx context.Context) error { return r.conn.Ping(ctx) }

// scanRows reads whatever columns came back. The column set comes from the table, not from a struct
// here, so a column an operator adds shows up without a code change and a projection needs no
// parallel type to describe it.
func scanRows(rows driver.Rows) ([]Span, error) {
	names := rows.Columns()
	types := rows.ColumnTypes()
	out := []Span{}
	for rows.Next() {
		dest := make([]any, len(names))
		for i := range dest {
			dest[i] = reflect.New(types[i].ScanType()).Interface()
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		span := Span{}
		for i, n := range names {
			span[n] = reflect.ValueOf(dest[i]).Elem().Interface()
		}
		out = append(out, span)
	}
	return out, rows.Err()
}
