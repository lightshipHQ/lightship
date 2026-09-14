package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lightshipHQ/lightship/internal/policy"
	"github.com/lightshipHQ/lightship/internal/schema"
)

// Each test owns an isolated schema within the disposable DATABASE_URL database.
func modelTestStores(t *testing.T) (*Store, func(pgx.QueryTracer) *Store) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("set DATABASE_URL to a disposable database to run model concurrency tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schemaName := "lightship_model_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schemaName}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quoted); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer admin.Close(ctx)
		if _, err := admin.Exec(ctx, "drop schema "+quoted+" cascade"); err != nil {
			t.Error(err)
		}
	})
	newStore := func(tracer pgx.QueryTracer) *Store {
		t.Helper()
		config, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		config.ConnConfig.RuntimeParams["search_path"] = schemaName
		config.ConnConfig.Tracer = tracer
		config.MaxConns = 4
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		st := &Store{pool: pool}
		t.Cleanup(st.Close)
		return st
	}
	st := newStore(nil)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st, newStore
}

func readTestModel(t *testing.T, st *Store) schema.Model {
	t.Helper()
	m, err := st.LoadModel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func assertPoliciesCompile(t *testing.T, m schema.Model) {
	t.Helper()
	env, err := policy.NewEnv(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range m.Roles {
		for _, p := range role.Policies {
			if _, err := env.Compile(p.Expression); err != nil {
				t.Fatalf("role %s no longer compiles: %v", role.Name, err)
			}
		}
	}
}

func TestUserAttributesBecomePolicyReferencesAutomatically(t *testing.T) {
	st, _ := modelTestStores(t)
	ctx := context.Background()
	if err := st.CreateUser(ctx, "automatic-attributes", "",
		map[string]string{"tenant_id": "acme"}, nil, false); err != nil {
		t.Fatal(err)
	}
	m := readTestModel(t, st)
	if !reflect.DeepEqual(m.UserAttrs, []string{"tenant_id"}) {
		t.Fatalf("user attributes = %v, want tenant_id", m.UserAttrs)
	}

	department := "support"
	if _, err := st.PatchAttributes(ctx, "automatic-attributes",
		map[string]*string{"department": &department}); err != nil {
		t.Fatal(err)
	}
	m = readTestModel(t, st)
	if !reflect.DeepEqual(m.UserAttrs, []string{"department", "tenant_id"}) {
		t.Fatalf("user attributes = %v, want automatic keys", m.UserAttrs)
	}

	if err := st.PutFields(ctx, nil, m.Version); err != nil {
		t.Fatal(err)
	}
	if got := readTestModel(t, st).UserAttrs; !reflect.DeepEqual(got, m.UserAttrs) {
		t.Fatalf("field save changed automatic user attributes: got %v, want %v", got, m.UserAttrs)
	}
}

func TestConcurrentModelProposalsRejectStaleValidation(t *testing.T) {
	a, newStore := modelTestStores(t)
	b := newStore(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	initial := readTestModel(t, a)
	if err := a.PutFields(ctx, []schema.Field{{Name: "Tenant", Policy: true}}, initial.Version); err != nil {
		t.Fatal(err)
	}
	left, right := readTestModel(t, a), readTestModel(t, b)
	left.Fields = nil
	role := schema.Role{Name: "tenant", Policies: []schema.Policy{{Title: "tenant", Expression: `Tenant == "acme"`}}}
	right.Roles = append(right.Roles, role)
	assertPoliciesCompile(t, left)
	assertPoliciesCompile(t, right)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- a.PutFields(ctx, nil, left.Version) }()
	go func() { <-start; results <- b.PutRole(ctx, role, right.Version) }()
	close(start)
	var successes, conflicts int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrModelConflict):
			conflicts++
		default:
			t.Fatalf("unexpected write error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want one of each", successes, conflicts)
	}
	actual := readTestModel(t, a)
	if actual.Version != left.Version+1 {
		t.Fatalf("version = %d, want %d", actual.Version, left.Version+1)
	}
	assertPoliciesCompile(t, actual)
}

func TestEveryModelMutationRejectsStaleVersionBeforeWriting(t *testing.T) {
	st, _ := modelTestStores(t)
	ctx := context.Background()
	role := schema.Role{Name: "existing", Policies: []schema.Policy{{Title: "allow", Expression: "true"}}}
	initial := readTestModel(t, st)
	if err := st.PutRole(ctx, role, initial.Version); err != nil {
		t.Fatal(err)
	}
	before := readTestModel(t, st)
	for name, mutate := range map[string]func() error{
		"binding": func() error { return st.SetBinding(ctx, schema.Binding{Table: "otel.changed"}, initial.Version) },
		"fields":  func() error { return st.PutFields(ctx, []schema.Field{{Name: "Changed"}}, initial.Version) },
		"role":    func() error { return st.PutRole(ctx, schema.Role{Name: "changed"}, initial.Version) },
		"delete":  func() error { return st.DeleteRole(ctx, "existing", initial.Version) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := mutate(); !errors.Is(err, ErrModelConflict) {
				t.Fatalf("got %v, want model conflict", err)
			}
			if after := readTestModel(t, st); !reflect.DeepEqual(before, after) {
				t.Fatalf("stale write changed model: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestBootstrapAdminInvalidatesEarlierModelProposals(t *testing.T) {
	st, newStore := modelTestStores(t)
	other := newStore(nil)
	ctx := context.Background()
	before := readTestModel(t, st)
	if _, err := other.EnsureAdmin(ctx, ""); err != nil {
		t.Fatal(err)
	}
	after := readTestModel(t, st)
	if after.Version != before.Version+1 {
		t.Fatalf("bootstrap version = %d, want %d", after.Version, before.Version+1)
	}
	if err := st.PutFields(ctx, nil, before.Version); !errors.Is(err, ErrModelConflict) {
		t.Fatalf("pre-bootstrap proposal got %v, want model conflict", err)
	}
}

type bindingReadKey struct{}

type bindingReadBarrier struct {
	read    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *bindingReadBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, bindingReadKey{}, strings.Contains(data.SQL, "from source_binding"))
}

func (b *bindingReadBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if isBinding, _ := ctx.Value(bindingReadKey{}).(bool); !isBinding {
		return
	}
	b.once.Do(func() {
		close(b.read)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	})
}

func TestLoadModelKeepsOneSnapshotDuringConcurrentCommit(t *testing.T) {
	writer, newStore := modelTestStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	initial := readTestModel(t, writer)
	if err := writer.SetBinding(ctx, schema.Binding{Table: "otel.before"}, initial.Version); err != nil {
		t.Fatal(err)
	}
	before := readTestModel(t, writer)
	barrier := &bindingReadBarrier{read: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(barrier.release) })
	defer release()
	reader := newStore(barrier)
	result := make(chan schema.Model, 1)
	errorsRead := make(chan error, 1)
	go func() {
		m, err := reader.LoadModel(ctx)
		result <- m
		errorsRead <- err
	}()
	select {
	case <-barrier.read:
	case <-ctx.Done():
		t.Fatal("reader did not reach binding read")
	}
	if err := writer.SetBinding(ctx, schema.Binding{Table: "otel.after"}, before.Version); err != nil {
		t.Fatal(err)
	}
	release()
	got := <-result
	if err := <-errorsRead; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("read mixed snapshots: got=%+v want=%+v", got, before)
	}
	after := readTestModel(t, reader)
	if after.Binding.Table != "otel.after" || after.Version != before.Version+1 {
		t.Fatalf("next read did not see committed update: %+v", after)
	}
}
