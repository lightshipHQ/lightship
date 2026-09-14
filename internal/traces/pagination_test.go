package traces

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/lightshipHQ/lightship/internal/policy"
	"github.com/lightshipHQ/lightship/internal/schema"
	"github.com/lightshipHQ/lightship/internal/testtable"
)

// Found against a real corpus: paging through 252 traces returned 243. The cursor carried a
// time.Time, and the driver marshalled a DateTime64(9) parameter down to second precision, so
// `(StartTime, TraceId) < cursor` silently dropped every trace sharing the cursor's second at a
// page boundary. Nine traces vanished between pages and nothing reported an error.
//
// Every trace here starts inside the same second, so a cursor that loses sub-second precision
// cannot page through them at all.
//
// Set LIGHTSHIP_TEST_CLICKHOUSE to run.
func TestScrollDoesNotSkipTracesWithinOneSecond(t *testing.T) {
	dsn := os.Getenv("LIGHTSHIP_TEST_CLICKHOUSE")
	if dsn == "" {
		t.Skip("set LIGHTSHIP_TEST_CLICKHOUSE to run")
	}
	const table = "otel.lightship_pagination_test"
	const traceCount = 40

	ctx := context.Background()
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	opts.Auth.Database = "default"
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	testtable.MustGuard(t, table)
	if err := conn.Exec(ctx, "CREATE DATABASE IF NOT EXISTS otel"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, `CREATE TABLE `+table+` (
		Timestamp DateTime64(9), TraceId String, SpanId String, ParentSpanId String,
		SpanName LowCardinality(String), ServiceName LowCardinality(String),
		Duration UInt64, StatusCode LowCardinality(String),
		ResourceAttributes Map(LowCardinality(String), String),
		SpanAttributes Map(LowCardinality(String), String)
	) ENGINE = MergeTree PARTITION BY toDate(Timestamp)
	  ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))`); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+table)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < traceCount; i++ {
		// Spread across one second: 40 traces, ~25ms apart, all sharing the same whole second.
		ts := base.Add(time.Duration(i) * 25 * time.Millisecond)
		if err := batch.Append(ts, fmt.Sprintf("tr%03d", i), fmt.Sprintf("sp%03d", i), "",
			"op", "svc", uint64(1000), "STATUS_CODE_OK",
			map[string]string{"tenant.id": "acme"}, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatal(err)
	}

	env, err := policy.NewEnv(schema.Model{
		UserAttrs: []string{"tenant_id"},
		Fields:    []schema.Field{{Map: "ResourceAttributes", Name: "tenant.id", Policy: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	prog, err := env.Compile(`ResourceAttributes["tenant.id"] == user.tenant_id`)
	if err != nil {
		t.Fatal(err)
	}
	r := New(conn)
	q := Query{
		Binding: schema.Binding{
			Table: table, TraceID: "TraceId", Timestamp: "Timestamp",
			SpanID: "SpanId", ParentSpanID: "ParentSpanId", Name: "SpanName",
		},
		Env: env,
		Caller: Caller{
			Username: "priya",
			Attrs:    map[string]string{"tenant_id": "acme"},
			Programs: []*policy.Program{prog},
		},
	}
	window := Window{From: base.Add(-time.Minute), To: base.Add(time.Minute)}

	seen := map[string]int{}
	var cur *Cursor
	for page := 0; page < 30; page++ {
		res, err := r.List(ctx, q, window, nil, cur, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range res.Traces {
			seen[tr.TraceID]++
		}
		if len(res.Traces) < 7 || res.Next == nil {
			break
		}
		cur = res.Next
	}

	if len(seen) != traceCount {
		t.Errorf("scrolled %d distinct traces, want %d — the cursor is skipping rows",
			len(seen), traceCount)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("trace %s returned %d times; a scroll must not repeat a row", id, n)
		}
	}
	_ = conn.Exec(ctx, "DROP TABLE IF EXISTS "+table)
}
