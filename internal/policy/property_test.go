package policy

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"

	"cel.dev/cel-go/cel"
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/lightshipHQ/lightship/internal/schema"
	"github.com/lightshipHQ/lightship/internal/testtable"
)

// The property this whole product rests on: the rows the translated SQL returns must equal the
// rows a direct CEL evaluation permits. Any disagreement is a leak or a wrongful denial, and
// neither is repairable after GA.
//
// The oracle is cel-go's own evaluator, which is not a coincidence. CEL raises an error when a map
// key is missing, and an error is not true — so "a span missing a declared attribute satisfies
// nothing" is already CEL's semantics. The SQL has to reproduce it with mapContains guards, and
// this test is what proves it does, including under negation where the guard is easy to get wrong.
//
// Set LIGHTSHIP_TEST_CLICKHOUSE to the DSN of a throwaway ClickHouse to run it.

const traceTable = "otel.lightship_property_test"

type span struct {
	trace string
	res   map[string]string
	spn   map[string]string
}

func TestPropertySQLAgreesWithCEL(t *testing.T) {
	dsn := os.Getenv("LIGHTSHIP_TEST_CLICKHOUSE")
	if dsn == "" {
		t.Skip("set LIGHTSHIP_TEST_CLICKHOUSE to run the property test")
	}
	ctx := context.Background()
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// The test creates its own database, so it must not connect to one that does not exist yet.
	// Every statement fully qualifies the table.
	opts.Auth.Database = "default"
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	rng := rand.New(rand.NewSource(20260828))
	spans := generateSpans(rng, 120, 4)
	if err := load(ctx, conn, spans); err != nil {
		t.Fatal(err)
	}

	m := schema.Model{
		UserAttrs: []string{"tenant_id"},
		Fields: []schema.Field{
			{Map: "ResourceAttributes", Name: "tenant.id", Policy: true},
			{Map: "ResourceAttributes", Name: "env", Policy: true},
			{Map: "SpanAttributes", Name: "agent.id", Policy: true},
		},
	}
	env, err := NewEnv(m)
	if err != nil {
		t.Fatal(err)
	}
	attrs := map[string]string{"tenant_id": "acme"}

	for i, src := range generatePolicies(rng, 200) {
		p, err := env.Compile(src)
		if err != nil {
			t.Fatalf("generated policy failed to compile: %s: %v", src, err)
		}
		pred, err := env.Predicate([]*Program{p}, attrs)
		having := Having(pred)
		q := Query{
			SQL:  "SELECT TraceId FROM " + traceTable + " GROUP BY TraceId HAVING " + having.SQL,
			Args: having.Args,
		}
		if err != nil {
			t.Fatalf("translate %s: %v", src, err)
		}

		got, err := queryTraces(ctx, conn, q)
		if err != nil {
			t.Fatalf("query for %s: %v\n%s", src, err, q.SQL)
		}
		want := referenceTraces(t, src, spans, attrs)

		if !equal(got, want) {
			t.Fatalf("case %d disagreed\npolicy: %s\nsql:    %s\nsql gave  %v\ncel gave  %v",
				i, src, q.SQL, got, want)
		}
	}
}

// referenceTraces evaluates the policy directly with cel-go, applying the `any` quantifier: a
// trace is permitted when at least one of its spans satisfies the policy. Which spans of it the
// caller may then read is a separate decision, made per span on the read path.
func referenceTraces(t *testing.T, src string, spans []span, attrs map[string]string) []string {
	t.Helper()
	strMap := cel.MapType(cel.StringType, cel.StringType)
	env, err := cel.NewEnv(
		cel.Variable("ResourceAttributes", strMap),
		cel.Variable("SpanAttributes", strMap),
		cel.Variable("user", strMap),
		cel.ClearMacros(),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(src)
	if iss != nil && iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}

	passed := map[string]bool{}
	seen := map[string]bool{}
	for _, s := range spans {
		seen[s.trace] = true
		out, _, err := prg.Eval(map[string]any{
			"ResourceAttributes": s.res, "SpanAttributes": s.spn, "user": attrs,
		})
		// An error is a missing key. Not true, therefore this span fails the policy.
		pass := false
		if err == nil {
			if b, isBool := out.Value().(bool); isBool {
				pass = b
			}
		}
		if pass {
			passed[s.trace] = true
		}
	}
	var out []string
	for tr := range seen {
		if passed[tr] {
			out = append(out, tr)
		}
	}
	sort.Strings(out)
	return out
}

func generateSpans(rng *rand.Rand, traces, maxSpans int) []span {
	tenants := []string{"acme", "globex", "initech"}
	envs := []string{"prod", "staging", "dev"}
	agents := []string{"a", "b", "c"}
	var out []span
	for i := 0; i < traces; i++ {
		id := fmt.Sprintf("t%04d", i)
		n := 1 + rng.Intn(maxSpans)
		for j := 0; j < n; j++ {
			res := map[string]string{}
			spn := map[string]string{}
			// Keys are present only sometimes. Absence is the case the guards exist for, so it
			// has to be common in the corpus rather than a rarity.
			if rng.Intn(4) > 0 {
				res["tenant.id"] = tenants[rng.Intn(len(tenants))]
			}
			if rng.Intn(4) > 0 {
				res["env"] = envs[rng.Intn(len(envs))]
			}
			if rng.Intn(4) > 0 {
				spn["agent.id"] = agents[rng.Intn(len(agents))]
			}
			out = append(out, span{trace: id, res: res, spn: spn})
		}
	}
	return out
}

func generatePolicies(rng *rand.Rand, n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, genExpr(rng, 0))
	}
	return out
}

func genExpr(rng *rand.Rand, depth int) string {
	if depth >= 2 || rng.Intn(3) == 0 {
		return genLeaf(rng)
	}
	switch rng.Intn(3) {
	case 0:
		return fmt.Sprintf("(%s && %s)", genExpr(rng, depth+1), genExpr(rng, depth+1))
	case 1:
		return fmt.Sprintf("(%s || %s)", genExpr(rng, depth+1), genExpr(rng, depth+1))
	default:
		return fmt.Sprintf("!(%s)", genExpr(rng, depth+1))
	}
}

func genLeaf(rng *rand.Rand) string {
	fields := []string{`ResourceAttributes["tenant.id"]`, `ResourceAttributes["env"]`, `SpanAttributes["agent.id"]`}
	vals := []string{`"acme"`, `"globex"`, `"prod"`, `"staging"`, `"a"`, `"b"`, `user.tenant_id`}
	f := fields[rng.Intn(len(fields))]
	switch rng.Intn(4) {
	case 0:
		return fmt.Sprintf(`%s != %s`, f, vals[rng.Intn(len(vals))])
	case 1:
		return fmt.Sprintf(`%s in ["a", "prod", "acme"]`, f)
	case 2:
		return fmt.Sprintf(`!(%s == %s)`, f, vals[rng.Intn(len(vals))])
	default:
		return fmt.Sprintf(`%s == %s`, f, vals[rng.Intn(len(vals))])
	}
}

// load owns its schema. The columns mirror what the OTel Collector's ClickHouse exporter creates,
// so the test is not coupled to whatever table happens to exist on the machine running it.
func load(ctx context.Context, conn clickhouse.Conn, spans []span) error {
	if err := testtable.Guard(traceTable); err != nil {
		return err
	}
	if err := conn.Exec(ctx, "CREATE DATABASE IF NOT EXISTS otel"); err != nil {
		return err
	}
	if err := conn.Exec(ctx, "DROP TABLE IF EXISTS "+traceTable); err != nil {
		return err
	}
	if err := conn.Exec(ctx, `CREATE TABLE `+traceTable+` (
		Timestamp DateTime64(9), TraceId String, SpanId String, ParentSpanId String,
		SpanName LowCardinality(String), ServiceName LowCardinality(String),
		Duration UInt64, StatusCode LowCardinality(String),
		ResourceAttributes Map(LowCardinality(String), String),
		SpanAttributes Map(LowCardinality(String), String)
	) ENGINE = MergeTree PARTITION BY toDate(Timestamp)
	  ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))`); err != nil {
		return err
	}
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+traceTable)
	if err != nil {
		return err
	}
	for i, s := range spans {
		if err := batch.Append(
			nowNanos(i), s.trace, fmt.Sprintf("s%06d", i), "",
			"op", "svc", uint64(1000), "STATUS_CODE_OK", s.res, s.spn,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

func queryTraces(ctx context.Context, conn clickhouse.Conn, q Query) ([]string, error) {
	rows, err := conn.Query(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func nowNanos(i int) time.Time {
	return time.Unix(1756000000, int64(i)).UTC()
}
