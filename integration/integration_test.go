// Package integration exercises the whole control plane against real Postgres and real ClickHouse:
// migration, setup, login, policy translation, the two-phase query, and the audit
// log — through HTTP, the way a caller reaches it.
//
// It seeds its own traces rather than reading an existing corpus, so the assertions are exact
// counts that do not drift as real data arrives. To browse a real corpus instead, point the binary
// at it by hand; see README.md.
//
// Skipped unless DATABASE_URL and LIGHTSHIP_TEST_CLICKHOUSE are set.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lightshipHQ/lightship/internal/auth"
	"github.com/lightshipHQ/lightship/internal/httpapi"
	"github.com/lightshipHQ/lightship/internal/model"
	"github.com/lightshipHQ/lightship/internal/store"
	"github.com/lightshipHQ/lightship/internal/testtable"
	"github.com/lightshipHQ/lightship/internal/traces"
)

const (
	table     = "otel.lightship_integration_test"
	adminPass = "adminpw"
)

// The seeded corpus. The last two entries are the ones the quantifier decides: a trace whose spans
// carry different tenants, and one whose second span carries none. Both are visible to whoever owns
// a span in them, and both come back whole.
var seeded = []struct {
	trace  string
	tenant []string // one entry per span
}{
	{"alok-1", []string{"alok", "alok", "alok"}},
	{"alok-2", []string{"alok"}},
	{"alok-3", []string{"alok", "alok"}},
	{"akanksha-1", []string{"akanksha", "akanksha"}},
	{"akanksha-2", []string{"akanksha"}},
	{"untagged-1", []string{"alok", ""}}, // second span carries no tenant.id at all
	{"mixed-1", []string{"alok", "akanksha"}},
}

// What each tenant can see under the ANY quantifier: their own traces, plus any trace they own a
// span in. Written once because it is the corpus's central fact and half the suite asserts it.
var (
	visibleToAlok     = []string{"alok-1", "alok-2", "alok-3", "untagged-1", "mixed-1"}
	visibleToAkanksha = []string{"akanksha-1", "akanksha-2", "mixed-1"}
)

const (
	// A trace is visible to whoever owns a span in it. alok owns one in untagged-1 and one in
	// mixed-1; akanksha owns one in mixed-1.
	wantAlok     = 5 // alok-1..3, untagged-1, mixed-1
	wantAkanksha = 3 // akanksha-1..2, mixed-1
	wantTotal    = 7
)

func TestFullStack(t *testing.T) {
	chDSN := os.Getenv("LIGHTSHIP_TEST_CLICKHOUSE")
	pgDSN := os.Getenv("DATABASE_URL")
	if chDSN == "" || pgDSN == "" {
		t.Skip("set DATABASE_URL and LIGHTSHIP_TEST_CLICKHOUSE to run")
	}
	ctx := context.Background()

	conn := seed(t, ctx, chDSN)
	defer conn.Close()

	st, err := store.Open(ctx, pgDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	adminHash, err := auth.Hash(adminPass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureAdmin(ctx, adminHash); err != nil {
		t.Fatal(err)
	}

	reader, err := traces.Open(chDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	// A short TTL so a subtest that changes the model and immediately queries is not testing the
	// cache's patience. Production defaults to a minute.
	models := model.New(st, time.Minute)
	srv := httptest.NewServer(httpapi.New(st, reader, models,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpapi.WithResourceLimits(httpapi.ResourceLimits{
			LoginConcurrency: 2, QueryConcurrency: 4,
			QueryTimeout: 5 * time.Second, MaxQueryWindow: 48 * time.Hour,
		})))
	defer srv.Close()

	admin := signIn(t, srv.URL, "admin", adminPass)

	// Everything below is setup through the API, because that is the only way to configure this
	// now — there is no config file. It doubles as the test of the admin flow.
	t.Run("setup", func(t *testing.T) {
		if code, body := do(t, admin, "PUT", srv.URL+"/schema/binding", fmt.Sprintf(`{
			"table": %q, "trace_id": "TraceId", "timestamp": "Timestamp",
			"span_id": "SpanId", "parent_span_id": "ParentSpanId", "name": "SpanName"}`,
			table)); code != http.StatusOK {
			t.Fatalf("bind schema: %d (%s)", code, body)
		}
		// A column the table does not have is caught before it is stored, rather than surfacing
		// as a raw ClickHouse error on somebody's next query.
		if code, body := do(t, admin, "PUT", srv.URL+"/schema/binding", fmt.Sprintf(
			`{"table":%q,"trace_id":"TraceId","timestamp":"NoSuchColumn"}`, table,
		)); code != http.StatusBadRequest {
			t.Errorf("binding a column that does not exist: got %d, want 400 (%s)", code, body)
		}
		if code, body := do(t, admin, "PUT", srv.URL+"/schema/fields", `{
			"fields": [{"map":"NoSuchMap","name":"x","filterable":true}]}`,
		); code != http.StatusBadRequest {
			t.Errorf("marking a map that does not exist: got %d, want 400 (%s)", code, body)
		}
		if code, body := do(t, admin, "PUT", srv.URL+"/schema/fields", `{
			"fields": [
			  {"map":"ResourceAttributes","name":"tenant.id","filterable":true,"policy":true},
			  {"name":"StatusCode","policy":true},
			  {"map":"ResourceAttributes","name":"deployment.environment","filterable":true},
			  {"map":"SpanAttributes","name":"gen_ai.request.model","filterable":true},
			  {"name":"ServiceName","filterable":true}
			]}`); code != http.StatusOK {
			t.Fatalf("mark fields: %d (%s)", code, body)
		}
		if code, body := do(t, admin, "PUT", srv.URL+"/schema/fields",
			`{"fields":[],"user_attributes":["tenant_id"]}`); code != http.StatusBadRequest {
			t.Errorf("manual attribute registration: got %d, want 400 (%s)", code, body)
		}
		// Two users holding the SAME role who see different data, because their attributes differ.
		// That is the mechanism worth testing: policy is caller-relative, not per-role.
		for _, u := range []struct{ name, pw, tenant string }{
			{"alok", "alokpw", "alok"},
			{"akanksha", "akankshapw", "akanksha"},
		} {
			h, err := auth.Hash(u.pw)
			if err != nil {
				t.Fatal(err)
			}
			do(t, admin, "DELETE", srv.URL+"/users/"+u.name, "") // the test database is reused
			// must_change_password is false because these two are fixtures the suite signs in as
			// repeatedly; the enrollment flow it would otherwise force has its own subtest below.
			if code, body := do(t, admin, "POST", srv.URL+"/users", fmt.Sprintf(
				`{"username":%q,"password_hash":%q,"attributes":{"tenant_id":%q},"roles":[],
				  "must_change_password":false}`,
				u.name, h, u.tenant)); code != http.StatusCreated {
				t.Fatalf("create %s: %d (%s)", u.name, code, body)
			}
		}
		if code, body := do(t, admin, "PUT", srv.URL+"/roles/own-tenant", `{"policies":[
			{"title":"own tenant only",
			 "expression":"ResourceAttributes[\"tenant.id\"] == user.tenant_id"}]}`,
		); code != http.StatusOK {
			t.Fatalf("create role: %d (%s)", code, body)
		}
		for _, username := range []string{"alok", "akanksha"} {
			if code, body := do(t, admin, "PATCH", srv.URL+"/users/"+username,
				`{"roles":["own-tenant"]}`); code != http.StatusOK {
				t.Fatalf("assign role to %s: %d (%s)", username, code, body)
			}
		}
	})

	alok := signIn(t, srv.URL, "alok", "alokpw")
	akanksha := signIn(t, srv.URL, "akanksha", "akankshapw")

	t.Run("admin can preview the interface as one role", func(t *testing.T) {
		code, body := sendPreview(t, admin, "own-tenant", "GET", srv.URL+"/me", "")
		if code != http.StatusOK {
			t.Fatalf("preview /me: %d (%s)", code, body)
		}
		var me struct {
			Username     string   `json:"username"`
			IsAdmin      bool     `json:"is_admin"`
			ActorIsAdmin bool     `json:"actor_is_admin"`
			PreviewRole  string   `json:"preview_role"`
			Roles        []string `json:"roles"`
		}
		if err := json.Unmarshal([]byte(body), &me); err != nil {
			t.Fatal(err)
		}
		if me.Username != "admin" || me.IsAdmin || !me.ActorIsAdmin ||
			me.PreviewRole != "own-tenant" || !slices.Equal(me.Roles, []string{"own-tenant"}) {
			t.Fatalf("unexpected preview identity: %+v", me)
		}

		window := fmt.Sprintf(`{"limit":100,"from":%q,"to":%q}`,
			time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339),
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		code, body = sendPreview(t, admin, "own-tenant", "POST", srv.URL+"/traces/query", window)
		if code != http.StatusOK {
			t.Fatalf("preview trace query: %d (%s)", code, body)
		}
		var page spanPage
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Spans) != 0 {
			t.Errorf("preview retained the admin bypass and returned %d spans", len(page.Spans))
		}
		if code, _ := sendPreview(t, admin, "own-tenant", "GET", srv.URL+"/schema", ""); code != http.StatusForbidden {
			t.Errorf("preview role retained admin UI access: got %d, want 403", code)
		}
		if code, _ := sendPreview(t, alok, "own-tenant", "GET", srv.URL+"/me", ""); code != http.StatusForbidden {
			t.Errorf("non-admin role preview: got %d, want 403", code)
		}
		if code, _ := sendPreview(t, admin, "missing-role", "GET", srv.URL+"/me", ""); code != http.StatusBadRequest {
			t.Errorf("unknown preview role: got %d, want 400", code)
		}
	})

	t.Run("two users, one role, different rows", func(t *testing.T) {
		a := list(t, alok, srv.URL)
		k := list(t, akanksha, srv.URL)

		assertTraces(t, "alok", a, visibleToAlok)
		assertTraces(t, "akanksha", k, visibleToAkanksha)

	})

	// A trace is the unit of access: it is returned when any of its spans satisfies the policy, and
	// it is returned whole. mixed-1 and untagged-1 are both visible to alok, complete — including
	// the untagged worker span, which is the kind of shared-infrastructure span that carries no
	// marker and is the one most worth seeing when something breaks.
	t.Run("a visible trace is returned whole", func(t *testing.T) {
		spansOf := func(c *http.Client, id string) int {
			t.Helper()
			var body struct {
				Spans   []map[string]any  `json:"spans"`
				Binding map[string]string `json:"binding"`
			}
			get(t, c, srv.URL+"/traces/"+id, &body)
			// The response carries the binding so a client can find the structural columns without
			// knowing the customer's table.
			if body.Binding["trace_id"] != "TraceId" {
				t.Errorf("detail must report the binding, got %v", body.Binding)
			}
			// Raw rows: every column of the table, not a subset lightship chose.
			if len(body.Spans) > 0 {
				for _, col := range []string{"TraceId", "SpanId", "StatusCode", "ResourceAttributes"} {
					if _, ok := body.Spans[0][col]; !ok {
						t.Errorf("column %s missing from a span row", col)
					}
				}
			}
			return len(body.Spans)
		}
		for id, want := range map[string]int{"alok-1": 3, "untagged-1": 2, "mixed-1": 2} {
			if got := spansOf(alok, id); got != want {
				t.Errorf("alok read %s and got %d spans, want %d", id, got, want)
			}
		}
		// The summary agrees with the read: no span is counted that is not returned.
		for _, tr := range list(t, alok, srv.URL).Traces {
			if tr.TraceID == "mixed-1" && tr.SpanCount != 2 {
				t.Errorf("mixed-1 summary reports %d spans, want 2", tr.SpanCount)
			}
		}
		// A trace no span of which satisfies alok's policy stays invisible.
		if code := status(t, alok, srv.URL+"/traces/akanksha-1"); code != http.StatusNotFound {
			t.Errorf("alok reading akanksha-1: got %d, want 404", code)
		}
	})

	// Denied and absent must be the same response, or the status code becomes an oracle for which
	// trace ids exist.
	t.Run("denied is indistinguishable from absent", func(t *testing.T) {
		real := status(t, akanksha, srv.URL+"/traces/alok-1")
		fake := status(t, akanksha, srv.URL+"/traces/does-not-exist-at-all")
		if real != http.StatusNotFound || fake != http.StatusNotFound {
			t.Errorf("existing-but-denied = %d, nonexistent = %d; both must be 404", real, fake)
		}
	})

	t.Run("the scroll returns every visible trace exactly once", func(t *testing.T) {
		seen := map[string]int{}
		cursor := ""
		for page := 0; page < 20; page++ {
			r := listPage(t, admin, srv.URL, cursor, 2)
			for _, tr := range r.Traces {
				seen[tr.TraceID]++
			}
			if r.Next == "" {
				break
			}
			cursor = r.Next
		}
		if len(seen) != wantTotal {
			t.Errorf("scrolled %d distinct traces, want %d", len(seen), wantTotal)
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("trace %s appeared %d times across pages", id, n)
			}
		}
	})

	t.Run("logout ends the session server-side", func(t *testing.T) {
		out := signIn(t, srv.URL, "alok", "alokpw")
		if code := readStatus(t, out, srv.URL); code != http.StatusOK {
			t.Fatalf("precondition: signed-in read got %d", code)
		}
		if code, body := do(t, out, "POST", srv.URL+"/logout", ""); code != http.StatusOK {
			t.Fatalf("logout: got %d (%s)", code, body)
		}
		// The cookie jar still holds a token; it has to stop working because the row is gone, not
		// because the browser was asked nicely to forget it.
		if code := readStatus(t, out, srv.URL); code != http.StatusUnauthorized {
			t.Errorf("read after logout: got %d, want 401", code)
		}
		// Signing out twice is not an error, but the second call has no session to authenticate.
		if code, _ := do(t, out, "POST", srv.URL+"/logout", ""); code != http.StatusUnauthorized {
			t.Errorf("second logout: got %d, want 401", code)
		}
	})

	t.Run("an API key cannot be logged out of", func(t *testing.T) {
		var k struct{ Token, ID string }
		_, body := do(t, alok, "POST", srv.URL+"/keys", `{"name":"logout-probe"}`)
		if err := json.Unmarshal([]byte(body), &k); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { do(t, admin, "DELETE", srv.URL+"/keys/"+k.ID, "") })
		if code, body := doKey(t, k.Token, "POST", srv.URL+"/logout", ""); code != http.StatusBadRequest {
			t.Errorf("logout with a key: got %d, want 400 (%s)", code, body)
		}
		// And the key still works, because nothing was revoked.
		if code, _ := doKey(t, k.Token, "GET", srv.URL+"/keys", ""); code != http.StatusOK {
			t.Error("the key must be unaffected by a rejected logout")
		}
	})

	// Provisioning. Users are no longer in the config file, so this is the surface that replaces it
	// — and the attribute call is the one that decides what a caller can see.
	t.Run("provisioning", func(t *testing.T) {
		hash, err := auth.Hash("newpw")
		if err != nil {
			t.Fatal(err)
		}

		t.Run("is admin-only", func(t *testing.T) {
			code, _ := do(t, alok, "POST", srv.URL+"/users",
				fmt.Sprintf(`{"username":"sneaky","password_hash":%q}`, hash))
			if code != http.StatusForbidden {
				t.Errorf("non-admin provisioning: got %d, want 403", code)
			}
		})

		t.Run("makes a new attribute available to policies", func(t *testing.T) {
			code, body := do(t, admin, "POST", srv.URL+"/users",
				fmt.Sprintf(`{"username":"bad","password_hash":%q,"attributes":{"department":"x"}}`, hash))
			if code != http.StatusCreated {
				t.Fatalf("new attribute: got %d, want 201 (%s)", code, body)
			}
			t.Cleanup(func() { do(t, admin, "DELETE", srv.URL+"/users/bad", "") })
			var schemaResponse struct {
				Model struct {
					UserAttrs []string `json:"user_attributes"`
				} `json:"model"`
			}
			get(t, admin, srv.URL+"/schema", &schemaResponse)
			if !slices.Contains(schemaResponse.Model.UserAttrs, "department") {
				t.Errorf("new attribute is not policy-referenceable: %v", schemaResponse.Model.UserAttrs)
			}
		})

		t.Run("refuses an attribute name policies cannot reference", func(t *testing.T) {
			code, body := do(t, admin, "POST", srv.URL+"/users",
				fmt.Sprintf(`{"username":"bad-name","password_hash":%q,"attributes":{"bad-key":"x"}}`, hash))
			if code != http.StatusBadRequest {
				t.Errorf("invalid attribute: got %d, want 400 (%s)", code, body)
			}
		})

		t.Run("refuses an unknown role", func(t *testing.T) {
			code, body := do(t, admin, "POST", srv.URL+"/users",
				fmt.Sprintf(`{"username":"bad2","password_hash":%q,"roles":["nope"]}`, hash))
			if code != http.StatusBadRequest {
				t.Errorf("unknown role: got %d, want 400 (%s)", code, body)
			}
		})

		if code, body := do(t, admin, "POST", srv.URL+"/users", fmt.Sprintf(
			`{"username":"provisioned","password_hash":%q,"attributes":{"tenant_id":"akanksha"},
			  "roles":["own-tenant"],"must_change_password":false}`,
			hash)); code != http.StatusCreated {
			t.Fatalf("create user: got %d, want 201 (%s)", code, body)
		}
		if code, _ := do(t, admin, "POST", srv.URL+"/users",
			fmt.Sprintf(`{"username":"provisioned","password_hash":%q}`, hash)); code != http.StatusConflict {
			t.Errorf("duplicate username: got %d, want 409", code)
		}

		t.Run("reads back what it wrote", func(t *testing.T) {
			var u struct {
				Username    string            `json:"username"`
				HasPassword bool              `json:"has_password"`
				Attributes  map[string]string `json:"attributes"`
				Roles       []string          `json:"roles"`
			}
			get(t, admin, srv.URL+"/users/provisioned", &u)
			if u.Attributes["tenant_id"] != "akanksha" || len(u.Roles) != 1 ||
				u.Roles[0] != "own-tenant" || !u.HasPassword {
				t.Errorf("round trip lost something: %+v", u)
			}
			var all struct {
				Users []struct {
					Username string `json:"username"`
				} `json:"users"`
			}
			get(t, admin, srv.URL+"/users", &all)
			if len(all.Users) < 4 { // admin, alok, akanksha, provisioned
				t.Errorf("list returned %d users, want at least 4", len(all.Users))
			}
		})

		// One role, one attribute, and the rows follow the attribute — which is the whole reason
		// provisioning can be O(rules) rather than O(people).
		newbie := signIn(t, srv.URL, "provisioned", "newpw")
		assertTraces(t, "provisioned", list(t, newbie, srv.URL), visibleToAkanksha)

		t.Run("changing an attribute changes what they see", func(t *testing.T) {
			if code, body := do(t, admin, "PATCH", srv.URL+"/users/provisioned/attributes",
				`{"tenant_id":"alok"}`); code != http.StatusOK {
				t.Fatalf("patch attributes: got %d (%s)", code, body)
			}
			// No new sign-in: roles and attributes are re-read on every request, so the change
			// applies to the session already open.
			assertTraces(t, "provisioned after re-tenanting", list(t, newbie, srv.URL), visibleToAlok)
		})

		// Removal has to be expressible, or a grant can be widened and never narrowed.
		t.Run("a null value removes the attribute", func(t *testing.T) {
			code, body := do(t, admin, "PATCH", srv.URL+"/users/provisioned/attributes",
				`{"tenant_id":null}`)
			if code != http.StatusOK {
				t.Fatalf("remove attribute: got %d (%s)", code, body)
			}
			if !strings.Contains(body, `"attributes":{}`) {
				t.Errorf("want an empty attribute set after removal, got %s", body)
			}
			// The policy compares tenant.id to an attribute the caller no longer has, so it matches
			// nothing — narrowing works, and it fails closed rather than open.
			if got := list(t, newbie, srv.URL); len(got.Traces) != 0 {
				t.Errorf("want no traces once the attribute is gone, got %d", len(got.Traces))
			}
			if code, body := do(t, admin, "PATCH", srv.URL+"/users/provisioned/attributes",
				`{"tenant_id":"alok"}`); code != http.StatusOK {
				t.Fatalf("restore attribute: got %d (%s)", code, body)
			}
		})

		t.Run("an empty patch is rejected", func(t *testing.T) {
			if code, _ := do(t, admin, "PATCH", srv.URL+"/users/provisioned/attributes",
				`{}`); code != http.StatusBadRequest {
				t.Errorf("empty patch: got %d, want 400", code)
			}
		})

		t.Run("removing every role removes every row", func(t *testing.T) {
			if code, body := do(t, admin, "PATCH", srv.URL+"/users/provisioned",
				`{"roles":[]}`); code != http.StatusOK {
				t.Fatalf("clear roles: got %d (%s)", code, body)
			}
			if got := list(t, newbie, srv.URL); len(got.Traces) != 0 {
				t.Errorf("a caller holding no role must see nothing, got %d traces", len(got.Traces))
			}
		})

		// The reason to reset a password is usually that someone else knows it, so the live
		// sessions have to go with it. Roles need no equivalent — they are re-read per request.
		t.Run("changing a password revokes that user's sessions", func(t *testing.T) {
			victim := signIn(t, srv.URL, "provisioned", "newpw")
			if code := readStatus(t, victim, srv.URL); code != http.StatusOK {
				t.Fatalf("precondition: signed-in read got %d", code)
			}
			h, err := auth.Hash("rotated")
			if err != nil {
				t.Fatal(err)
			}
			code, body := do(t, admin, "PATCH", srv.URL+"/users/provisioned",
				fmt.Sprintf(`{"password_hash":%q}`, h))
			if code != http.StatusOK {
				t.Fatalf("rotate password: %d (%s)", code, body)
			}
			if !strings.Contains(body, `"sessions_revoked"`) {
				t.Errorf("the response must say how many sessions went: %s", body)
			}
			if code := readStatus(t, victim, srv.URL); code != http.StatusUnauthorized {
				t.Errorf("session after a password change: got %d, want 401", code)
			}
			reset := signIn(t, srv.URL, "provisioned", "rotated")
			var me meResponse
			get(t, reset, srv.URL+"/me", &me)
			if !me.MustChange {
				t.Fatal("an administrator-supplied password must re-arm enrollment")
			}
			if code := readStatus(t, reset, srv.URL); code != http.StatusForbidden {
				t.Errorf("reading with reset password: got %d, want 403", code)
			}
			if code, _ := do(t, reset, "POST", srv.URL+"/keys", `{"name":"before-reset-completed"}`); code != http.StatusForbidden {
				t.Errorf("minting a key with reset password: got %d, want 403", code)
			}
			if code, body := do(t, reset, "POST", srv.URL+"/me/password",
				`{"current_password":"rotated","new_password":"owner-chosen-password"}`); code != http.StatusOK {
				t.Fatalf("complete password reset: %d (%s)", code, body)
			}
			get(t, reset, srv.URL+"/me", &me)
			if me.MustChange {
				t.Error("self-service password change must clear enrollment")
			}
			if code := readStatus(t, reset, srv.URL); code != http.StatusOK {
				t.Errorf("reading after completing reset: got %d, want 200", code)
			}
		})

		t.Run("removing a password does not re-arm enrollment or revoke API keys", func(t *testing.T) {
			owner := signIn(t, srv.URL, "provisioned", "owner-chosen-password")
			code, body := do(t, owner, "POST", srv.URL+"/keys", `{"name":"password-removal"}`)
			if code != http.StatusCreated {
				t.Fatalf("create API key: %d (%s)", code, body)
			}
			var key struct{ Token string }
			if err := json.Unmarshal([]byte(body), &key); err != nil {
				t.Fatal(err)
			}
			if code, body := do(t, admin, "PATCH", srv.URL+"/users/provisioned",
				`{"password_hash":""}`); code != http.StatusOK {
				t.Fatalf("remove password: %d (%s)", code, body)
			}
			if code := status(t, owner, srv.URL+"/me"); code != http.StatusUnauthorized {
				t.Errorf("session after removing password: got %d, want 401", code)
			}
			code, body = doKey(t, key.Token, "GET", srv.URL+"/me", "")
			if code != http.StatusOK {
				t.Fatalf("API key after removing password: %d (%s)", code, body)
			}
			var me meResponse
			if err := json.Unmarshal([]byte(body), &me); err != nil {
				t.Fatal(err)
			}
			if me.MustChange {
				t.Error("removing a password must not re-arm enrollment")
			}
		})

		// Deprovisioning, which the config file could not express at all: the row goes, and the
		// session goes with it rather than lapsing on its own schedule.
		t.Run("delete revokes the live session", func(t *testing.T) {
			if code, body := do(t, admin, "DELETE", srv.URL+"/users/provisioned", ""); code != http.StatusOK {
				t.Fatalf("delete user: got %d (%s)", code, body)
			}
			if code := readStatus(t, newbie, srv.URL); code != http.StatusUnauthorized {
				t.Errorf("session after delete: got %d, want 401", code)
			}
		})

		t.Run("the bootstrap admin cannot be deleted through the API", func(t *testing.T) {
			if code, _ := do(t, admin, "DELETE", srv.URL+"/users/admin", ""); code != http.StatusForbidden {
				t.Errorf("deleting admin: got %d, want 403", code)
			}
		})
	})

	// Enrollment. An admin who computes a user's hash knows that user's password forever and can
	// sign in as them, and every audit row such a session writes names the wrong actor — which is
	// the one claim this product cannot afford to lose. So the server generates the password, shows
	// it to the admin once, and refuses the account everything until its owner replaces it.
	t.Run("generated password and forced change", func(t *testing.T) {
		do(t, admin, "DELETE", srv.URL+"/users/enrolled", "") // the test database is reused
		t.Cleanup(func() { do(t, admin, "DELETE", srv.URL+"/users/enrolled", "") })

		var created struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		code, body := do(t, admin, "POST", srv.URL+"/users",
			`{"username":"enrolled","attributes":{"tenant_id":"alok"},"roles":["own-tenant"]}`)
		if code != http.StatusCreated {
			t.Fatalf("create without a hash: got %d, want 201 (%s)", code, body)
		}
		if err := json.Unmarshal([]byte(body), &created); err != nil {
			t.Fatal(err)
		}
		if created.Password == "" {
			t.Fatal("omitting password_hash must return a generated password")
		}

		// The password is readable exactly once. Nothing else ever shows it, and the stored form is
		// a hash — an admin who loses it reprovisions rather than recovers it.
		t.Run("the generated password appears nowhere else", func(t *testing.T) {
			var u struct {
				Username    string `json:"username"`
				HasPassword bool   `json:"has_password"`
			}
			get(t, admin, srv.URL+"/users/enrolled", &u)
			if !u.HasPassword {
				t.Error("a generated password must be stored")
			}
			var raw map[string]any
			get(t, admin, srv.URL+"/users/enrolled", &raw)
			for k, v := range raw {
				if str, ok := v.(string); ok && str == created.Password {
					t.Errorf("the password is readable again through %q", k)
				}
			}
		})

		enrolled := signIn(t, srv.URL, "enrolled", created.Password)

		t.Run("me reports the pending state", func(t *testing.T) {
			var me meResponse
			get(t, enrolled, srv.URL+"/me", &me)
			if me.Username != "enrolled" || me.IsAdmin || !me.MustChange {
				t.Errorf("GET /me on a pending user: %+v", me)
			}
		})

		// The point of the flag: the credential the admin read cannot be used for anything but
		// replacing itself, so it is a handover rather than a shared secret.
		t.Run("a pending user is refused everything else", func(t *testing.T) {
			if code := readStatus(t, enrolled, srv.URL); code != http.StatusForbidden {
				t.Errorf("pending user querying traces: got %d, want 403", code)
			}
			code, body := do(t, enrolled, "POST", srv.URL+"/keys", `{"name":"too-early"}`)
			if code != http.StatusForbidden {
				t.Errorf("pending user minting a key: got %d, want 403", code)
			}
			if !strings.Contains(body, `"must_change_password":true`) {
				t.Errorf("the refusal must say why: %s", body)
			}
			if code, _ := do(t, enrolled, "GET", srv.URL+"/filter/schema", ""); code != http.StatusForbidden {
				t.Errorf("pending user reading the filter schema: got %d, want 403", code)
			}
			if code, _ := do(t, enrolled, "GET", srv.URL+"/users", ""); code != http.StatusForbidden {
				t.Errorf("pending user reaching an admin route: got %d, want 403", code)
			}
		})

		// A second session on the same account, opened before the change, to prove it dies with the
		// password it was started on.
		stale := signIn(t, srv.URL, "enrolled", created.Password)

		t.Run("the change verifies the current password", func(t *testing.T) {
			if code, _ := do(t, enrolled, "POST", srv.URL+"/me/password",
				`{"current_password":"wrong","new_password":"a-new-password"}`); code != http.StatusForbidden {
				t.Errorf("wrong current password: got %d, want 403", code)
			}
			if code, _ := do(t, enrolled, "POST", srv.URL+"/me/password", fmt.Sprintf(
				`{"current_password":%q,"new_password":"short"}`,
				created.Password)); code != http.StatusBadRequest {
				t.Errorf("a too-short new password: got %d, want 400", code)
			}
		})

		code, body = do(t, enrolled, "POST", srv.URL+"/me/password", fmt.Sprintf(
			`{"current_password":%q,"new_password":"a-new-password"}`, created.Password))
		if code != http.StatusOK {
			t.Fatalf("change password: got %d, want 200 (%s)", code, body)
		}
		if !strings.Contains(body, `"sessions_revoked"`) {
			t.Errorf("the response must say how many sessions went: %s", body)
		}

		t.Run("the change clears the flag and opens the product", func(t *testing.T) {
			var me meResponse
			get(t, enrolled, srv.URL+"/me", &me)
			if me.MustChange {
				t.Error("the flag survived the change")
			}
			assertTraces(t, "enrolled", list(t, enrolled, srv.URL), visibleToAlok)
			if code, body := do(t, enrolled, "POST", srv.URL+"/keys", `{"name":"now-allowed"}`); code != http.StatusCreated {
				t.Errorf("minting a key after the change: got %d, want 201 (%s)", code, body)
			}
		})

		// A password change is usually a response to the old one being known by someone else, so
		// the sessions started on it have to go — the caller's own excepted, since signing them out
		// of the page they just used would read as a failure.
		t.Run("the other sessions die with the old password", func(t *testing.T) {
			if code := status(t, stale, srv.URL+"/me"); code != http.StatusUnauthorized {
				t.Errorf("a session older than the change: got %d, want 401", code)
			}
			if code := loginStatus(t, srv.URL, "enrolled", created.Password); code != http.StatusUnauthorized {
				t.Errorf("the generated password after the change: got %d, want 401", code)
			}
		})

		// An identity nobody signs into interactively would never perform the change, so forcing it
		// would leave the account permanently unable to mint the key it exists to hold.
		t.Run("must_change_password false is honoured", func(t *testing.T) {
			do(t, admin, "DELETE", srv.URL+"/users/service", "")
			t.Cleanup(func() { do(t, admin, "DELETE", srv.URL+"/users/service", "") })
			var svc struct {
				Password string `json:"password"`
			}
			code, body := do(t, admin, "POST", srv.URL+"/users",
				`{"username":"service","attributes":{"tenant_id":"alok"},"roles":["own-tenant"],
				  "must_change_password":false}`)
			if code != http.StatusCreated {
				t.Fatalf("create a service account: got %d (%s)", code, body)
			}
			if err := json.Unmarshal([]byte(body), &svc); err != nil {
				t.Fatal(err)
			}
			c := signIn(t, srv.URL, "service", svc.Password)
			var me meResponse
			get(t, c, srv.URL+"/me", &me)
			if me.MustChange {
				t.Error("must_change_password: false was not honoured")
			}
			assertTraces(t, "service", list(t, c, srv.URL), visibleToAlok)
		})

		// The bootstrap admin generates its own credential and nobody else ever reads it, so there
		// is no handover to complete and no reason to lock it out of its own setup.
		t.Run("the bootstrap admin is not flagged", func(t *testing.T) {
			var me meResponse
			get(t, admin, srv.URL+"/me", &me)
			if me.Username != "admin" || !me.IsAdmin || me.MustChange {
				t.Errorf("GET /me as the bootstrap admin: %+v", me)
			}
		})
	})

	// Unattended provisioning — a script, CI, a Terraform provider — runs as a service account: an
	// ordinary user holding the admin role that nobody signs into interactively. There is no
	// user-less credential; the account signs in once to mint its own key, and from then on it has
	// the same scoping, revocation and audit attribution as everyone else.
	t.Run("service-account keys", func(t *testing.T) {
		saHash, err := auth.Hash("provpw")
		if err != nil {
			t.Fatal(err)
		}
		// The test database is reused between runs, so this test removes what it creates.
		do(t, admin, "DELETE", srv.URL+"/users/provisioner", "")
		t.Cleanup(func() {
			do(t, admin, "DELETE", srv.URL+"/users/by-key", "")
			do(t, admin, "DELETE", srv.URL+"/users/provisioner", "")
		})
		if code, body := do(t, admin, "POST", srv.URL+"/users", fmt.Sprintf(
			`{"username":"provisioner","password_hash":%q,"roles":["admin"],
			  "must_change_password":false}`, saHash,
		)); code != http.StatusCreated {
			t.Fatalf("create service account: got %d (%s)", code, body)
		}

		var key struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		prov := signIn(t, srv.URL, "provisioner", "provpw")
		code, body := do(t, prov, "POST", srv.URL+"/keys", `{"name":"provisioner-key"}`)
		if code != http.StatusCreated {
			t.Fatalf("create key: got %d (%s)", code, body)
		}
		if err := json.Unmarshal([]byte(body), &key); err != nil {
			t.Fatal(err)
		}
		if key.Token == "" || !auth.LooksLikeKey(key.Token) {
			t.Fatalf("want a token with the %s prefix, got %q", auth.KeyPrefix, key.Token)
		}

		hash, err := auth.Hash("keypw")
		if err != nil {
			t.Fatal(err)
		}

		t.Run("provisions like an admin", func(t *testing.T) {
			code, body := doKey(t, key.Token, "POST", srv.URL+"/users", fmt.Sprintf(
				`{"username":"by-key","password_hash":%q,"attributes":{"tenant_id":"alok"},
				  "roles":["own-tenant"],"must_change_password":false}`,
				hash))
			if code != http.StatusCreated {
				t.Fatalf("create via key: got %d (%s)", code, body)
			}
			assertTraces(t, "by-key", list(t, signIn(t, srv.URL, "by-key", "keypw"), srv.URL), visibleToAlok)
		})

		// A read made with a key must name the key. Attributing it to the username alone would put
		// the wrong actor in the log: a key would be indistinguishable from that user sitting at a
		// browser.
		t.Run("a key-driven read names the key in the audit log", func(t *testing.T) {
			if code, _ := doKey(t, key.Token, "POST", srv.URL+"/traces/query", `{"limit":1}`); code != http.StatusOK {
				t.Fatal("read via key failed")
			}
			var body struct {
				Entries []struct {
					Username string         `json:"username"`
					Action   string         `json:"action"`
					Detail   map[string]any `json:"detail"`
				} `json:"entries"`
			}
			get(t, admin, srv.URL+"/audit?limit=50", &body)
			var found bool
			for _, e := range body.Entries {
				if e.Action == "query.traces" && e.Username == "provisioner" &&
					e.Detail["via_key"] == "provisioner-key" {
					found = true
				}
			}
			if !found {
				t.Error("a trace read made with an API key must record via_key")
			}
		})

		// Revocation has to bite at the moment of the call, not whenever something is re-read.
		t.Run("revocation takes effect immediately", func(t *testing.T) {
			if code, _ := do(t, admin, "DELETE", srv.URL+"/keys/"+key.ID, ""); code != http.StatusOK {
				t.Fatal("revoke failed")
			}
			if code, _ := doKey(t, key.Token, "GET", srv.URL+"/users", ""); code != http.StatusUnauthorized {
				t.Errorf("revoked key: got %d, want 401", code)
			}
		})

		t.Run("a bad key is not a session", func(t *testing.T) {
			if code, _ := doKey(t, auth.KeyPrefix+"nonsense", "GET", srv.URL+"/users", ""); code != http.StatusUnauthorized {
				t.Errorf("bogus key: got %d, want 401", code)
			}
		})

		// First-run setup depends on this: the bootstrap admin is the only user that exists, and
		// connecting an agent means handing it a key. The ErrProtected guards keep the account
		// itself immutable through the API; minting a key acts as the account without changing it.
		t.Run("the bootstrap admin can mint its own key", func(t *testing.T) {
			var k struct {
				ID       string `json:"id"`
				Token    string `json:"token"`
				Username string `json:"username"`
			}
			code, body := do(t, admin, "POST", srv.URL+"/keys", `{"name":"root","expires_in":"24h"}`)
			if code != http.StatusCreated {
				t.Fatalf("bootstrap admin minting a key: got %d, want 201 (%s)", code, body)
			}
			if err := json.Unmarshal([]byte(body), &k); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { do(t, admin, "DELETE", srv.URL+"/keys/"+k.ID, "") })
			if k.Username != "admin" {
				t.Errorf("the key must act as the admin, got %q", k.Username)
			}
			if code, _ := doKey(t, k.Token, "GET", srv.URL+"/users", ""); code != http.StatusOK {
				t.Error("the admin's key must reach an admin route")
			}
		})
	})

	// The API and MCP are the primary surface, so a caller has to be able to mint their own
	// credential without asking an admin for one.
	t.Run("user keys", func(t *testing.T) {
		var k struct{ Token, ID, Username string }
		code, body := do(t, alok, "POST", srv.URL+"/keys", `{"name":"alok-cli"}`)
		if code != http.StatusCreated {
			t.Fatalf("self-service key: got %d (%s)", code, body)
		}
		if err := json.Unmarshal([]byte(body), &k); err != nil {
			t.Fatal(err)
		}
		if k.Username != "alok" {
			t.Errorf("a key with no target must belong to the caller, got %q", k.Username)
		}
		t.Cleanup(func() { do(t, admin, "DELETE", srv.URL+"/keys/"+k.ID, "") })

		keyList := func() listResult {
			t.Helper()
			code, body := doKey(t, k.Token, "POST", srv.URL+"/traces/query", fmt.Sprintf(
				`{"limit":100,"from":%q,"to":%q}`,
				time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339),
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
			if code != http.StatusOK {
				t.Fatalf("read via key: %d (%s)", code, body)
			}
			var p spanPage
			if err := json.Unmarshal([]byte(body), &p); err != nil {
				t.Fatal(err)
			}
			return summarise(p)
		}

		assertTraces(t, "alok's key", keyList(), visibleToAlok)

		// The point of the design: the key holds no grant of its own. Change the user and the same
		// token, unmodified and never re-minted, resolves differently on its next request.
		t.Run("follows the user's attributes by reference", func(t *testing.T) {
			t.Cleanup(func() {
				do(t, admin, "PATCH", srv.URL+"/users/alok/attributes", `{"tenant_id":"alok"}`)
			})
			if code, body := do(t, admin, "PATCH", srv.URL+"/users/alok/attributes",
				`{"tenant_id":"akanksha"}`); code != http.StatusOK {
				t.Fatalf("re-tenant alok: %d (%s)", code, body)
			}
			assertTraces(t, "alok's key after re-tenanting", keyList(), visibleToAkanksha)
		})

		t.Run("follows the user's roles by reference", func(t *testing.T) {
			t.Cleanup(func() {
				do(t, admin, "PATCH", srv.URL+"/users/alok", `{"roles":["own-tenant"]}`)
			})
			if code, _ := do(t, admin, "PATCH", srv.URL+"/users/alok", `{"roles":[]}`); code != http.StatusOK {
				t.Fatal("clear alok's roles")
			}
			if got := keyList(); len(got.Traces) != 0 {
				t.Errorf("a key must lose what its user lost, got %d traces", len(got.Traces))
			}
		})

		// A key is always minted for the caller. The fields that once named another principal are
		// gone from the request schema entirely, so impersonation-by-minting is unrepresentable
		// rather than merely forbidden — for an admin exactly as for anyone else.
		t.Run("cannot mint a key for another principal", func(t *testing.T) {
			for who, c := range map[string]*http.Client{"non-admin": alok, "admin": admin} {
				for field, payload := range map[string]string{
					"system":   `{"name":"x","system":true}`,
					"username": `{"name":"x","username":"akanksha"}`,
				} {
					if code, body := do(t, c, "POST", srv.URL+"/keys", payload); code != http.StatusBadRequest {
						t.Errorf("%s field as %s: got %d, want 400 (%s)", field, who, code, body)
					}
				}
			}
		})

		t.Run("sees only its own keys", func(t *testing.T) {
			var mine struct {
				Keys []struct {
					Username string `json:"username"`
				} `json:"keys"`
			}
			get(t, alok, srv.URL+"/keys", &mine)
			for _, key := range mine.Keys {
				if key.Username != "alok" {
					t.Errorf("a non-admin must not enumerate other principals' keys: %+v", key)
				}
			}
		})

		t.Run("cannot revoke another principal's key", func(t *testing.T) {
			var other struct{ ID string }
			_, body := do(t, akanksha, "POST", srv.URL+"/keys", `{"name":"not-alok"}`)
			if err := json.Unmarshal([]byte(body), &other); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { do(t, admin, "DELETE", srv.URL+"/keys/"+other.ID, "") })
			// 404 rather than 403: whether someone else's key exists is not a fact to leak.
			if code, _ := do(t, alok, "DELETE", srv.URL+"/keys/"+other.ID, ""); code != http.StatusNotFound {
				t.Errorf("revoking another principal's key: got %d, want 404", code)
			}
		})
	})

	// Discovery reports what is in ClickHouse and decides nothing. Everything lightship knows about
	// the customer's table came through here.
	t.Run("discovery", func(t *testing.T) {
		var out struct {
			Tables []struct {
				Database, Table  string
				Score            int
				SuggestedBinding struct {
					Table     string `json:"table"`
					TraceID   string `json:"trace_id"`
					Timestamp string `json:"timestamp"`
					Name      string `json:"name"`
				} `json:"suggested_binding"`
				SuggestedFields []struct {
					Name       string `json:"name"`
					Filterable bool   `json:"filterable"`
				} `json:"suggested_fields"`
			} `json:"tables"`
			Keys []struct {
				Map, Key       string
				Spans          uint64
				Coverage       float64
				DistinctValues uint64 `json:"distinct_values"`
			} `json:"keys"`
		}
		code, body := do(t, admin, "POST", srv.URL+"/schema/discover", fmt.Sprintf(
			`{"table":%q,"timestamp":"Timestamp","maps":["ResourceAttributes","SpanAttributes"],"window":"24h"}`,
			table))
		if code != http.StatusOK {
			t.Fatalf("discover: %d (%s)", code, body)
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Keys) == 0 {
			t.Fatalf("no keys in discovery response: %s", body)
		}

		var found bool
		for _, c := range out.Tables {
			if c.Database+"."+c.Table == table {
				found = true
				// The binding is suggested by column name, never assumed — but on a standard
				// layout the suggestion should be the whole thing.
				if c.SuggestedBinding.TraceID != "TraceId" || c.SuggestedBinding.Timestamp != "Timestamp" ||
					c.SuggestedBinding.Name != "SpanName" {
					t.Errorf("suggested binding is incomplete: %+v", c.SuggestedBinding)
				}
				// Service is not a role, but a recognisable service column should arrive
				// pre-suggested for the field-marking screen.
				var service bool
				for _, f := range c.SuggestedFields {
					if f.Name == "ServiceName" && f.Filterable {
						service = true
					}
				}
				if !service {
					t.Errorf("ServiceName is not among the suggested fields: %+v", c.SuggestedFields)
				}
			}
		}
		if !found {
			t.Errorf("discovery did not report the seeded table %s", table)
		}

		// Coverage counts spans carrying the key, not visible traces. tenant.id is on every seeded
		// span but one; that span's trace remains visible through its other, matching span.
		var tenant bool
		for _, k := range out.Keys {
			if k.Map == "ResourceAttributes" && k.Key == "tenant.id" {
				tenant = true
				if k.Coverage <= 0 || k.Coverage > 1 {
					t.Errorf("coverage out of range: %v", k.Coverage)
				}
				if k.DistinctValues != 2 { // alok and akanksha
					t.Errorf("distinct values = %d, want 2", k.DistinctValues)
				}
			}
		}
		if !tenant {
			t.Error("discovery did not find ResourceAttributes['tenant.id']")
		}
	})

	t.Run("filter schema", func(t *testing.T) {
		type field struct {
			Ref    string `json:"ref"`
			Policy *bool  `json:"policy"`
		}
		var mine struct {
			Binding struct {
				TraceID      string `json:"trace_id"`
				Timestamp    string `json:"timestamp"`
				SpanID       string `json:"span_id"`
				ParentSpanID string `json:"parent_span_id"`
				Name         string `json:"name"`
			}
			Fields []field
		}
		get(t, alok, srv.URL+"/filter/schema", &mine)
		if mine.Binding.TraceID == "" || mine.Binding.Timestamp == "" || mine.Binding.SpanID == "" ||
			mine.Binding.ParentSpanID == "" || mine.Binding.Name == "" {
			t.Errorf("filter schema omitted structural binding: %+v", mine.Binding)
		}
		if len(mine.Fields) == 0 {
			t.Fatal("a caller must be told what they may filter on")
		}
		for _, f := range mine.Fields {
			// Which fields carry the security model is an admin's business.
			if f.Policy != nil {
				t.Errorf("%s exposed its policy flag to a non-admin", f.Ref)
			}
		}
		var theirs struct{ Fields []field }
		get(t, admin, srv.URL+"/filter/schema", &theirs)
		var sawPolicy bool
		for _, f := range theirs.Fields {
			if f.Policy != nil && *f.Policy {
				sawPolicy = true
			}
		}
		if !sawPolicy {
			t.Error("an admin must see which fields a policy may reference")
		}
	})

	// The model is editable at runtime now, so the thing to prove is that editing it changes what
	// callers see — and that a bad edit changes nothing at all.
	t.Run("policy changes take effect", func(t *testing.T) {
		t.Cleanup(func() {
			do(t, admin, "PUT", srv.URL+"/roles/own-tenant", `{"policies":[
				{"title":"own tenant only",
				 "expression":"ResourceAttributes[\"tenant.id\"] == user.tenant_id"}]}`)
		})

		// Narrow the role: same caller, same session, fewer rows.
		if code, body := do(t, admin, "PUT", srv.URL+"/roles/own-tenant", `{"policies":[
			{"title":"own tenant only",
			 "expression":"ResourceAttributes[\"tenant.id\"] == \"nobody-at-all\""}]}`,
		); code != http.StatusOK {
			t.Fatalf("narrow the policy: %d (%s)", code, body)
		}
		if got := list(t, alok, srv.URL); len(got.Traces) != 0 {
			t.Errorf("want no traces after narrowing, got %d", len(got.Traces))
		}
	})

	t.Run("an invalid policy is rejected and changes nothing", func(t *testing.T) {
		before := list(t, alok, srv.URL)
		for name, expr := range map[string]string{
			"undeclared field": `ResourceAttributes["nope"] == "x"`,
			"unmarked column":  `TraceState == "x"`,
			"a function":       `size(ResourceAttributes) == 1`,
		} {
			code, body := do(t, admin, "PUT", srv.URL+"/roles/own-tenant",
				fmt.Sprintf(`{"policies":[{"title":"bad","expression":"%s"}]}`, expr))
			if code != http.StatusBadRequest {
				t.Errorf("%s: got %d, want 400 (%s)", name, code, body)
			}
		}
		// The running system is untouched — which is the whole point of compiling before storing,
		// where a config file would have made the same typo stop the next boot.
		after := list(t, alok, srv.URL)
		if len(after.Traces) != len(before.Traces) {
			t.Errorf("a rejected write changed what a caller sees: %d then %d",
				len(before.Traces), len(after.Traces))
		}
	})

	// The dangerous edit: unmark a field some policy still references. The policy would stop
	// constraining and nobody would be told, so the write has to be refused.
	t.Run("a field a policy references cannot be unmarked", func(t *testing.T) {
		before := list(t, alok, srv.URL)
		code, body := do(t, admin, "PUT", srv.URL+"/schema/fields", `{
			"fields":[{"map":"ResourceAttributes","name":"deployment.environment","filterable":true}]}`)
		if code != http.StatusBadRequest {
			t.Errorf("unmarking a referenced field: got %d, want 400 (%s)", code, body)
		}
		if after := list(t, alok, srv.URL); len(after.Traces) != len(before.Traces) {
			t.Errorf("a refused write changed what a caller sees: %d then %d",
				len(before.Traces), len(after.Traces))
		}
	})

	t.Run("field changes preserve automatic user attributes", func(t *testing.T) {
		var before struct {
			Model struct {
				Fields    json.RawMessage `json:"fields"`
				UserAttrs []string        `json:"user_attributes"`
			} `json:"model"`
		}
		get(t, admin, srv.URL+"/schema", &before)
		code, body := do(t, admin, "PUT", srv.URL+"/schema/fields",
			fmt.Sprintf(`{"fields":%s}`, before.Model.Fields))
		if code != http.StatusOK {
			t.Fatalf("saving fields: got %d, want 200 (%s)", code, body)
		}
		var after struct {
			Model struct {
				UserAttrs []string `json:"user_attributes"`
			} `json:"model"`
		}
		get(t, admin, srv.URL+"/schema", &after)
		if !slices.Equal(before.Model.UserAttrs, after.Model.UserAttrs) {
			t.Errorf("field change altered user attributes: before %v, after %v",
				before.Model.UserAttrs, after.Model.UserAttrs)
		}
	})

	// The filter. Its whole safety argument is that it narrows an already authorized set, so the
	// tests that matter are the ones that would catch it widening.
	t.Run("filter", func(t *testing.T) {
		query := func(c *http.Client, body string) listResult {
			t.Helper()
			return summarise(fetchSpans(t, c, srv.URL, body))
		}
		win := fmt.Sprintf(`"from":%q,"to":%q`,
			time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339),
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))

		// The one that matters. alok names akanksha's tenant in a filter; a filter that widened
		// would hand him traces the policy never permitted.
		//
		// mixed-1 coming back is correct, not a leak: he owns a span in it, so it was already in
		// his visible set and already returned whole without any filter. What must never appear is
		// a trace no span of which satisfies his policy.
		t.Run("cannot widen past the policy", func(t *testing.T) {
			got := query(alok, fmt.Sprintf(`{%s,"filter":{"conditions":[
				{"map":"ResourceAttributes","name":"tenant.id","op":"eq","value":"akanksha"}]}}`, win))
			visible := map[string]bool{}
			for _, id := range visibleToAlok {
				visible[id] = true
			}
			for _, tr := range got.Traces {
				if !visible[tr.TraceID] {
					t.Errorf("a filter reached %s, which the policy does not permit", tr.TraceID)
				}
			}
		})

		t.Run("narrows within what the policy allows", func(t *testing.T) {
			got := query(alok, fmt.Sprintf(`{%s,"filter":{"conditions":[
				{"map":"ResourceAttributes","name":"tenant.id","op":"eq","value":"alok"}]}}`, win))
			assertTraces(t, "alok filtered to his own tenant", got, visibleToAlok)
		})

		// Only the second span of each trace carries opus, so a single-span trace has none. The
		// multi-span ones match because one of their spans does.
		t.Run("matches a trace when any span matches", func(t *testing.T) {
			got := query(alok, fmt.Sprintf(`{%s,"filter":{"conditions":[
				{"map":"SpanAttributes","name":"gen_ai.request.model","op":"eq","value":"claude-opus-5"}]}}`, win))
			// alok-2 is a single span and drops out; everything else alok can see has a second span.
			assertTraces(t, "any-span match", got,
				[]string{"alok-1", "alok-3", "untagged-1", "mixed-1"})
		})

		t.Run("conditions AND together", func(t *testing.T) {
			got := query(alok, fmt.Sprintf(`{%s,"filter":{"conditions":[
				{"map":"ResourceAttributes","name":"tenant.id","op":"eq","value":"alok"},
				{"map":"SpanAttributes","name":"gen_ai.request.model","op":"eq","value":"claude-opus-5"}]}}`, win))
			assertTraces(t, "both conditions", got, []string{"alok-1", "alok-3"})
		})

		t.Run("prefix matches, and a wildcard is not a wildcard", func(t *testing.T) {
			got := query(alok, fmt.Sprintf(`{%s,"filter":{"conditions":[
				{"map":"ResourceAttributes","name":"tenant.id","op":"prefix","value":"alo"}]}}`, win))
			assertTraces(t, "prefix", got, visibleToAlok)

			// startsWith, not LIKE: a caller-supplied % must be a literal percent sign, or the
			// narrow operator becomes "contains anything" and an enumeration primitive.
			got = query(alok, fmt.Sprintf(`{%s,"filter":{"conditions":[
				{"map":"ResourceAttributes","name":"tenant.id","op":"prefix","value":"%%"}]}}`, win))
			if len(got.Traces) != 0 {
				t.Errorf("%% behaved as a wildcard: %d traces", len(got.Traces))
			}
		})

		t.Run("in, ne and exists", func(t *testing.T) {
			got := query(alok, fmt.Sprintf(`{%s,"filter":{"conditions":[
				{"map":"ResourceAttributes","name":"tenant.id","op":"in","values":["alok","nobody"]}]}}`, win))
			assertTraces(t, "in", got, visibleToAlok)

			got = query(alok, fmt.Sprintf(`{%s,"filter":{"conditions":[
				{"map":"SpanAttributes","name":"gen_ai.request.model","op":"ne","value":"claude-opus-5"}]}}`, win))
			// Every trace has a sonnet span, so ne matches them all under ANY.
			assertTraces(t, "ne", got, visibleToAlok)

			// untagged-1's worker span carries no tenant at all, so it is the one trace with a
			// span for which the key is absent.
			got = query(alok, fmt.Sprintf(`{%s,"filter":{"conditions":[
				{"map":"ResourceAttributes","name":"tenant.id","op":"not_exists"}]}}`, win))
			assertTraces(t, "not_exists", got, []string{"untagged-1"})
		})

		t.Run("a field that is not filterable is refused", func(t *testing.T) {
			code, body := do(t, alok, "POST", srv.URL+"/traces/query", fmt.Sprintf(
				`{%s,"filter":{"conditions":[{"name":"StatusCode","op":"eq","value":"STATUS_CODE_OK"}]}}`, win))
			if code != http.StatusBadRequest {
				t.Errorf("policy-only field in a filter: got %d, want 400 (%s)", code, body)
			}
			code, body = do(t, alok, "POST", srv.URL+"/traces/query", fmt.Sprintf(
				`{%s,"filter":{"conditions":[{"map":"ResourceAttributes","name":"never.declared","op":"eq","value":"x"}]}}`, win))
			if code != http.StatusBadRequest {
				t.Errorf("undeclared field in a filter: got %d, want 400 (%s)", code, body)
			}
		})

		t.Run("a cursor belongs to its filter", func(t *testing.T) {
			first := query(admin, fmt.Sprintf(`{%s,"limit":2,"filter":{"conditions":[
				{"map":"ResourceAttributes","name":"deployment.environment","op":"eq","value":"dev"}]}}`, win))
			if first.Next == "" {
				t.Fatal("expected a full page to carry a cursor")
			}
			// Same cursor, different filter: the scroll would silently repeat and skip.
			code, body := do(t, admin, "POST", srv.URL+"/traces/query", fmt.Sprintf(
				`{%s,"limit":2,"cursor":%q,"filter":{"conditions":[
				{"map":"ResourceAttributes","name":"tenant.id","op":"eq","value":"alok"}]}}`,
				win, first.Next))
			if code != http.StatusBadRequest {
				t.Errorf("cursor reused under a different filter: got %d, want 400 (%s)", code, body)
			}
		})
	})

	// MCP goes through the same query path as everything else, so what is worth testing is that it
	// really does — especially that a tool call cannot reach past the caller's policy.
	t.Run("mcp", func(t *testing.T) {
		rpc := func(c *http.Client, method, params string) map[string]any {
			t.Helper()
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)
			code, raw := do(t, c, "POST", srv.URL+"/mcp", body)
			if code != http.StatusOK {
				t.Fatalf("%s: HTTP %d (%s)", method, code, raw)
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				t.Fatal(err)
			}
			if e, ok := out["error"]; ok {
				t.Fatalf("%s: rpc error %v", method, e)
			}
			return out["result"].(map[string]any)
		}
		// The text payload of a tool result, which is where the answer actually lives.
		toolText := func(res map[string]any) (string, bool) {
			t.Helper()
			content := res["content"].([]any)
			text := content[0].(map[string]any)["text"].(string)
			isErr, _ := res["isError"].(bool)
			return text, isErr
		}

		t.Run("initialize and list tools", func(t *testing.T) {
			init := rpc(alok, "initialize", `{}`)
			if init["protocolVersion"] == "" {
				t.Error("initialize must report a protocol version")
			}
			// The spec's channel for telling a client how the server is meant to be used, and the
			// only place the division of labour between MCP and HTTP can be stated.
			instr, _ := init["instructions"].(string)
			for _, want := range []string{"/traces/query", "next_cursor", "context window",
				"lightship_filter_schema"} {
				if !strings.Contains(instr, want) {
					t.Errorf("initialize instructions must mention %q:\n%s", want, instr)
				}
			}
			tools := rpc(alok, "tools/list", `{}`)["tools"].([]any)
			names := map[string]bool{}
			for _, tl := range tools {
				names[tl.(map[string]any)["name"].(string)] = true
			}
			for _, want := range []string{"lightship_filter_schema", "lightship_search_traces",
				"lightship_get_trace"} {
				if !names[want] {
					t.Errorf("tool %s is missing", want)
				}
			}
			// The search tool must say where rows come from; an agent that reads only descriptions
			// would otherwise page summaries forever, or ask for a trace at a time.
			for _, tl := range tools {
				m := tl.(map[string]any)
				if m["name"] == "lightship_search_traces" &&
					!strings.Contains(m["description"].(string), "/traces/query") {
					t.Error("the search tool must point at the HTTP read for rows")
				}
			}
			// The escape hatch every comparable product has, and the one we can never adopt.
			for n := range names {
				if strings.Contains(n, "sql") {
					t.Errorf("a raw-SQL tool would bypass the policy: %s", n)
				}
			}
		})

		t.Run("a search is restricted to the caller's policy", func(t *testing.T) {
			res := rpc(alok, "tools/call",
				`{"name":"lightship_search_traces","arguments":{"limit":20}}`)
			text, isErr := toolText(res)
			if isErr {
				t.Fatalf("search failed: %s", text)
			}
			// akanksha's own traces contain no span of alok's, so nothing makes them visible.
			for _, denied := range []string{"akanksha-1", "akanksha-2"} {
				if strings.Contains(text, denied) {
					t.Errorf("MCP returned %s to alok", denied)
				}
			}
			if !strings.Contains(text, "alok-1") {
				t.Errorf("MCP returned none of alok's traces: %s", text)
			}
			// mixed-1 is visible to him — he owns a span in it — and comes back whole.
			trace, _ := toolText(rpc(alok, "tools/call",
				`{"name":"lightship_get_trace","arguments":{"trace_id":"mixed-1"}}`))
			if !strings.Contains(trace, "sp011") || !strings.Contains(trace, "sp012") {
				t.Errorf("a visible trace must come back whole:\n%s", trace)
			}
		})

		t.Run("a filter through MCP cannot widen either", func(t *testing.T) {
			res := rpc(alok, "tools/call", `{"name":"lightship_search_traces","arguments":{
				"filter":{"conditions":[
					{"map":"ResourceAttributes","name":"tenant.id","op":"eq","value":"akanksha"}]}}}`)
			text, _ := toolText(res)
			if strings.Contains(text, "akanksha-") {
				t.Errorf("a filtered MCP search reached denied rows: %s", text)
			}
		})

		t.Run("a denied trace read is indistinguishable from a missing one", func(t *testing.T) {
			denied, _ := toolText(rpc(alok, "tools/call",
				`{"name":"lightship_get_trace","arguments":{"trace_id":"akanksha-1"}}`))
			missing, _ := toolText(rpc(alok, "tools/call",
				`{"name":"lightship_get_trace","arguments":{"trace_id":"no-such-trace"}}`))
			if denied != missing {
				t.Errorf("the two answers differ, which makes the tool an existence oracle:\n%s\n%s",
					denied, missing)
			}
		})

		t.Run("search never reports denied trace counts", func(t *testing.T) {
			for _, args := range []string{
				`{}`,
				`{"excluded":true}`,
				`{"excluded":true,"filter":{"conditions":[{"map":"ResourceAttributes","name":"tenant.id","op":"prefix","value":"zzz"}]}}`,
			} {
				text, isErr := toolText(rpc(alok, "tools/call", fmt.Sprintf(
					`{"name":"lightship_search_traces","arguments":%s}`, args)))
				if isErr {
					t.Fatalf("search: %s", text)
				}
				var d map[string]any
				if err := json.Unmarshal([]byte(text), &d); err != nil {
					t.Fatal(err)
				}
				if _, ok := d["excluded_by_policy"]; ok {
					t.Errorf("search revealed denied trace counts: %s", text)
				}
			}
		})

		t.Run("the schema tool describes only what is filterable", func(t *testing.T) {
			text, _ := toolText(rpc(alok, "tools/call",
				`{"name":"lightship_filter_schema","arguments":{}}`))
			if !strings.Contains(text, "prefix") || !strings.Contains(text, "tenant.id") {
				t.Errorf("filter schema looks empty: %s", text)
			}
			if strings.Contains(text, "StatusCode") {
				t.Error("a policy-only field was advertised as filterable")
			}
		})

		t.Run("a notification gets no body", func(t *testing.T) {
			code, body := do(t, alok, "POST", srv.URL+"/mcp",
				`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			if code != http.StatusAccepted || strings.TrimSpace(body) != "" {
				t.Errorf("notification answered with %d %q", code, body)
			}
		})

		t.Run("MCP needs a credential", func(t *testing.T) {
			code, _ := doKey(t, auth.KeyPrefix+"nonsense", "POST", srv.URL+"/mcp",
				`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
			if code != http.StatusUnauthorized {
				t.Errorf("unauthenticated MCP: got %d, want 401", code)
			}
		})
	})

	// Suggestions come from the marked set: marking is the deliberate, stable statement of which
	// fields matter, and an index is far too expensive to create on the cadence policies change.
	t.Run("optimization suggestions", func(t *testing.T) {
		var out struct {
			Table       string                                         `json:"table"`
			SortingKey  string                                         `json:"sorting_key"`
			DDL         string                                         `json:"ddl"`
			Suggestions []struct{ Target, Status, Reason, DDL string } `json:"suggestions"`
		}
		get(t, admin, srv.URL+"/schema/optimizations", &out)
		if out.Table != table {
			t.Errorf("reported table %q, want %q", out.Table, table)
		}

		byTarget := map[string]string{}
		for _, sg := range out.Suggestions {
			byTarget[sg.Target] = sg.Status
		}
		// The seeded table carries no skip indexes, so every marked map wants one — including the
		// map only a filter names, because marking is the declaration and the filter's placement
		// may change.
		for _, m := range []string{"ResourceAttributes", "SpanAttributes"} {
			if byTarget[m] != "missing" {
				t.Errorf("%s: status %q, want missing", m, byTarget[m])
			}
		}
		// ServiceName leads the sorting key, so a comparison on it is already a range scan.
		if byTarget["ServiceName"] != "present" {
			t.Errorf("ServiceName: status %q, want present — it is in %q",
				byTarget["ServiceName"], out.SortingKey)
		}
		// The DDL must be applicable as written, including the backfill ADD INDEX omits.
		for _, want := range []string{"ALTER TABLE " + table, "mapValues(ResourceAttributes)",
			"mapValues(SpanAttributes)", "bloom_filter", "MATERIALIZE INDEX"} {
			if !strings.Contains(out.DDL, want) {
				t.Errorf("DDL is missing %q:\n%s", want, out.DDL)
			}
		}

		t.Run("is admin-only", func(t *testing.T) {
			if code := status(t, alok, srv.URL+"/schema/optimizations"); code != http.StatusForbidden {
				t.Errorf("non-admin: got %d, want 403", code)
			}
		})
	})

	// Setup MCP dispatches through the same handlers curl reaches, so a write through a tool call is
	// indistinguishable from a write through REST. Its endpoint and tool inventory remain separate
	// from everyday trace analysis.
	t.Run("admin mcp", func(t *testing.T) {
		call := func(c *http.Client, name, args string) (string, bool) {
			t.Helper()
			body := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
				name, args)
			code, raw := do(t, c, "POST", srv.URL+"/mcp/setup", body)
			if code != http.StatusOK {
				t.Fatalf("%s: HTTP %d (%s)", name, code, raw)
			}
			var out struct {
				Result struct {
					Content []struct{ Text string } `json:"content"`
					IsError bool                    `json:"isError"`
				} `json:"result"`
				Error *struct{ Message string } `json:"error"`
			}
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				t.Fatal(err)
			}
			if out.Error != nil {
				t.Fatalf("%s: rpc error %s", name, out.Error.Message)
			}
			return out.Result.Content[0].Text, out.Result.IsError
		}

		t.Run("tool lists are separated by endpoint", func(t *testing.T) {
			names := func(c *http.Client, path string) map[string]bool {
				t.Helper()
				code, raw := do(t, c, "POST", srv.URL+path,
					`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
				if code != http.StatusOK {
					t.Fatalf("tools/list: HTTP %d (%s)", code, raw)
				}
				var out struct {
					Result struct {
						Tools []struct {
							Name string `json:"name"`
						} `json:"tools"`
					} `json:"result"`
				}
				if err := json.Unmarshal([]byte(raw), &out); err != nil {
					t.Fatal(err)
				}
				set := map[string]bool{}
				for _, tl := range out.Result.Tools {
					set[tl.Name] = true
				}
				return set
			}
			analysis, setup := names(admin, "/mcp"), names(admin, "/mcp/setup")
			if !analysis["lightship_search_traces"] || analysis["lightship_create_user"] {
				t.Errorf("analysis tool list crossed surfaces: %v", analysis)
			}
			if !setup["lightship_create_user"] || setup["lightship_search_traces"] {
				t.Errorf("setup tool list crossed surfaces: %v", setup)
			}
		})

		t.Run("setup endpoint refuses a non-admin", func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
				`{"name":"lightship_delete_user","arguments":{"username":"akanksha"}}}`
			code, raw := do(t, alok, "POST", srv.URL+"/mcp/setup", body)
			if code != http.StatusForbidden || !strings.Contains(raw, "admin only") {
				t.Errorf("non-admin setup call: HTTP %d %s", code, raw)
			}
			// Nothing happened: the named user still exists and can sign in.
			signIn(t, srv.URL, "akanksha", "akankshapw")
		})

		// A user provisioned through MCP is a user, with the same policy behaviour.
		t.Run("a write through a tool is a write", func(t *testing.T) {
			h, err := auth.Hash("mcppw")
			if err != nil {
				t.Fatal(err)
			}
			do(t, admin, "DELETE", srv.URL+"/users/viamcp", "")
			if text, isErr := call(admin, "lightship_create_user", fmt.Sprintf(
				`{"username":"viamcp","password_hash":%q,"attributes":{"tenant_id":"akanksha"},
				  "roles":["own-tenant"],"must_change_password":false}`,
				h)); isErr {
				t.Fatalf("create through MCP: %s", text)
			}
			t.Cleanup(func() { do(t, admin, "DELETE", srv.URL+"/users/viamcp", "") })
			assertTraces(t, "user made through MCP",
				list(t, signIn(t, srv.URL, "viamcp", "mcppw"), srv.URL), visibleToAkanksha)

			// The unwrapped-body case: PATCH /users/{u}/attributes takes the map itself.
			if text, isErr := call(admin, "lightship_set_attributes",
				`{"username":"viamcp","attributes":{"tenant_id":"alok"}}`); isErr {
				t.Fatalf("set attributes through MCP: %s", text)
			}
			var u struct {
				Attributes map[string]string `json:"attributes"`
			}
			get(t, admin, srv.URL+"/users/viamcp", &u)
			if u.Attributes["tenant_id"] != "alok" {
				t.Errorf("attributes after the tool call: %v", u.Attributes)
			}
		})

		// Validation is the handler's, not the tool's, so an invalid policy fails identically.
		t.Run("validation flows through", func(t *testing.T) {
			text, isErr := call(admin, "lightship_put_role",
				`{"name":"bad","policies":[{"title":"t","expression":"NoSuchColumn == \"a\""}]}`)
			if !isErr || !strings.Contains(text, "undeclared reference") {
				t.Errorf("an invalid policy must fail the same way it does over REST: %s", text)
			}
			if text, isErr := call(admin, "lightship_update_user", `{"username":"alok"}`); !isErr ||
				!strings.Contains(text, "at least one of") {
				t.Errorf("a call with no fields must say so: %s", text)
			}
		})
	})

	// The bulk read: rows rather than summaries, paginated by the same scroll, so pagination is the
	// export. This is what an agent curls to a file.
	t.Run("span list", func(t *testing.T) {
		win := fmt.Sprintf(`"from":%q,"to":%q`,
			time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339),
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		fetch := func(c *http.Client, extra string) spanPage {
			t.Helper()
			return fetchSpans(t, c, srv.URL, fmt.Sprintf("{%s%s}", win, extra))
		}

		// Rows, with every column, the same shape a trace read returns.
		all := fetch(alok, `,"limit":50`)
		if len(all.Spans) == 0 {
			t.Fatal("no spans returned")
		}
		for _, col := range []string{"TraceId", "SpanId", "StatusCode", "ResourceAttributes"} {
			if _, ok := all.Spans[0][col]; !ok {
				t.Errorf("column %s missing from a row", col)
			}
		}

		// The policy applies exactly as it does to the summary list.
		seen := map[string]bool{}
		for _, sp := range all.Spans {
			seen[sp["TraceId"].(string)] = true
		}
		for _, id := range visibleToAlok {
			if !seen[id] {
				t.Errorf("%s is visible to alok but absent from the span list", id)
			}
		}
		for _, denied := range []string{"akanksha-1", "akanksha-2"} {
			if seen[denied] {
				t.Errorf("the span list returned %s to alok", denied)
			}
		}

		// The second query re-applies the policy rather than trusting the trace ids the first one
		// produced. Same result, different thing to rely on: hand that statement any id and it
		// returns nothing unless a span of that trace satisfies the caller's policy.
		t.Run("the row query is self-authorizing, not id-trusting", func(t *testing.T) {
			mine := fetch(alok, `,"limit":50,"columns":["TraceId"]`)
			theirs := fetch(akanksha, `,"limit":50,"columns":["TraceId"]`)
			ids := func(p spanPage) map[string]bool {
				out := map[string]bool{}
				for _, sp := range p.Spans {
					out[sp["TraceId"].(string)] = true
				}
				return out
			}
			a, k := ids(mine), ids(theirs)
			// akanksha-1 is hers alone; alok-1 is his. Neither appears in the other's rows, and
			// mixed-1 is in both because each owns a span of it.
			if a["akanksha-1"] || a["akanksha-2"] {
				t.Error("alok received rows of a trace no span of which is his")
			}
			if k["alok-1"] || k["alok-2"] || k["alok-3"] || k["untagged-1"] {
				t.Error("akanksha received rows of a trace no span of which is hers")
			}
			if !a["mixed-1"] || !k["mixed-1"] {
				t.Error("a trace both own a span of must be visible to both")
			}
		})

		t.Run("columns projects, and omitting it returns everything", func(t *testing.T) {
			p := fetch(alok, `,"limit":50,"columns":["TraceId","SpanId"]`)
			for _, sp := range p.Spans {
				if len(sp) != 2 {
					t.Fatalf("projection returned %d columns: %v", len(sp), sp)
				}
			}
		})

		// An identifier that is not one never reaches the SQL text.
		t.Run("a column name that is not an identifier is refused", func(t *testing.T) {
			for _, bad := range []string{"Span Id", "a'b", "1) OR 1=1--"} {
				body, _ := json.Marshal(map[string]any{"columns": []string{bad}})
				if code, _ := do(t, alok, "POST", srv.URL+"/traces/query", string(body)); code != http.StatusBadRequest {
					t.Errorf("columns=%q: got %d, want 400", bad, code)
				}
			}
		})

		// limit counts traces, not spans, because the cursor is a trace position.
		t.Run("pagination is by trace and walks the same scroll", func(t *testing.T) {
			first := fetch(alok, `,"limit":2`)
			if first.Next == "" {
				t.Fatal("a full page must carry a cursor")
			}
			traces := map[string]bool{}
			for _, sp := range first.Spans {
				traces[sp["TraceId"].(string)] = true
			}
			if len(traces) != 2 {
				t.Errorf("limit=2 returned spans from %d traces, want 2", len(traces))
			}
			p2 := fetch(alok, fmt.Sprintf(`,"limit":2,"cursor":%q`, first.Next))
			for _, sp := range p2.Spans {
				if traces[sp["TraceId"].(string)] {
					t.Errorf("page 2 repeated trace %s", sp["TraceId"])
				}
			}
		})
	})

	t.Run("audit is admin-only and records the decision", func(t *testing.T) {
		// The guard is the adminOnly wrapper on the route, like every other admin route.
		if code := status(t, alok, srv.URL+"/audit"); code != http.StatusForbidden {
			t.Errorf("non-admin reading /audit: got %d, want 403", code)
		}
		var body struct {
			Entries []struct {
				Username string         `json:"username"`
				Action   string         `json:"action"`
				Detail   map[string]any `json:"detail"`
			} `json:"entries"`
		}
		get(t, admin, srv.URL+"/audit?limit=200", &body)

		var denied bool
		for _, e := range body.Entries {
			if e.Username == "akanksha" && e.Action == "query.detail" &&
				e.Detail["trace_id"] == "alok-1" && e.Detail["visible"] == false {
				denied = true
			}
		}
		if !denied {
			t.Error("the audit log must record the denied read of alok-1 by akanksha")
		}
	})

	// Every role is required: each is something a span table carries by definition, so a binding
	// missing one is rejected at the write rather than stored and worked around. The message is
	// for an operator — it must say which roles are missing and where to fix them.
	t.Run("a binding missing required roles is rejected", func(t *testing.T) {
		code, body := do(t, admin, "PUT", srv.URL+"/schema/binding", fmt.Sprintf(
			`{"table":%q,"trace_id":"TraceId","timestamp":"Timestamp"}`, table))
		if code != http.StatusBadRequest {
			t.Fatalf("binding without span_id, parent_span_id and name: got %d, want 400 (%s)",
				code, body)
		}
		for _, role := range []string{"span_id", "parent_span_id", "name"} {
			if !strings.Contains(body, role) {
				t.Errorf("the rejection must name the missing role %q: %s", role, body)
			}
		}
		// The rejected write must leave the stored binding serving queries.
		if got := list(t, admin, srv.URL); len(got.Traces) == 0 {
			t.Fatal("a rejected binding must not disturb the stored one")
		}
	})
}

// ── helpers ─────────────────────────────────────────────────────────────────

func modelFor(spanIndex int) string {
	if spanIndex == 1 {
		return "claude-opus-5"
	}
	return "claude-sonnet-5"
}

func seed(t *testing.T, ctx context.Context, dsn string) clickhouse.Conn {
	t.Helper()
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	testtable.MustGuard(t, table)
	opts.Auth.Database = "default" // the test creates its database; it cannot connect into it
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE DATABASE IF NOT EXISTS otel",
		"DROP TABLE IF EXISTS " + table,
		`CREATE TABLE ` + table + ` (
			Timestamp DateTime64(9), TraceId String, SpanId String, ParentSpanId String,
			SpanName LowCardinality(String), ServiceName LowCardinality(String),
			Duration UInt64, StatusCode LowCardinality(String),
			ResourceAttributes Map(LowCardinality(String), String),
			SpanAttributes Map(LowCardinality(String), String)
		) ENGINE = MergeTree PARTITION BY toDate(Timestamp)
		  ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))`,
	} {
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+table)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).UTC()
	n := 0
	for _, s := range seeded {
		for i, tenant := range s.tenant {
			res := map[string]string{"deployment.environment": "dev"}
			if tenant != "" {
				res["tenant.id"] = tenant
			}
			parent := ""
			if i > 0 {
				parent = fmt.Sprintf("sp%03d", n-1)
			}
			// All within one second, so a cursor that loses sub-second precision cannot page.
			if err := batch.Append(
				base.Add(time.Duration(n)*20*time.Millisecond), s.trace,
				fmt.Sprintf("sp%03d", n), parent, "op", "svc",
				uint64(1_000_000), "STATUS_CODE_OK", res,
				// Only the second span of a trace carries opus. A filter for it must match a
				// trace where ANY span does, even when the other spans do not match.
				map[string]string{"gen_ai.request.model": modelFor(i)},
			); err != nil {
				t.Fatal(err)
			}
			n++
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatal(err)
	}
	return conn
}

// listResult is what the tests assert on: the distinct traces a page of rows contains, in the
// order the server returned them, with each one's span count. The server returns rows now — a
// summary is the client's to compute, so the tests compute it the same way the UI does.
type listResult struct {
	Traces []struct {
		TraceID   string
		SpanCount int
	}
	Next string
}

func summarise(page spanPage) listResult {
	var out listResult
	at := map[string]int{}
	for _, sp := range page.Spans {
		id, _ := sp[page.Binding["trace_id"]].(string)
		if i, seen := at[id]; seen {
			out.Traces[i].SpanCount++
			continue
		}
		at[id] = len(out.Traces)
		out.Traces = append(out.Traces, struct {
			TraceID   string
			SpanCount int
		}{id, 1})
	}
	out.Next = page.Next
	return out
}

type spanPage struct {
	Spans   []map[string]any  `json:"spans"`
	Binding map[string]string `json:"binding"`
	Next    string            `json:"next_cursor"`
}

// readStatus probes whether a credential can read at all. The read is a POST, so this cannot use
// the plain GET helper.
func readStatus(t *testing.T, c *http.Client, base string) int {
	t.Helper()
	code, _ := do(t, c, "POST", base+"/traces/query", `{"limit":1}`)
	return code
}

// meResponse is GET /me: who the caller is, and the two facts a client would otherwise have to
// infer by provoking a refusal.
type meResponse struct {
	Username   string `json:"username"`
	IsAdmin    bool   `json:"is_admin"`
	MustChange bool   `json:"must_change_password"`
}

// loginStatus attempts a sign-in and reports only the status, for the cases where the interesting
// answer is a refusal.
func loginStatus(t *testing.T, base, user, pass string) int {
	t.Helper()
	resp, err := (&http.Client{}).Post(base+"/login", "application/json",
		stringsReader(fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func signIn(t *testing.T, base, user, pass string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass)
	resp, err := c.Post(base+"/login", "application/json", stringsReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in as %s: %d", user, resp.StatusCode)
	}
	return c
}

func list(t *testing.T, c *http.Client, base string) listResult {
	t.Helper()
	return listPage(t, c, base, "", 100)
}

func listPage(t *testing.T, c *http.Client, base, cursor string, limit int) listResult {
	t.Helper()
	return summarise(fetchSpans(t, c, base, fmt.Sprintf(
		`{"limit":%d,"from":%q,"to":%q%s}`, limit,
		time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339),
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		cursorField(cursor))))
}

func cursorField(cursor string) string {
	if cursor == "" {
		return ""
	}
	return fmt.Sprintf(`,"cursor":%q`, cursor)
}

func fetchSpans(t *testing.T, c *http.Client, base, body string) spanPage {
	t.Helper()
	code, raw := do(t, c, "POST", base+"/traces/query", body)
	if code != http.StatusOK {
		t.Fatalf("POST /traces/query: %d %s", code, raw)
	}
	var p spanPage
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// do issues a mutating request on a signed-in client and returns the status and raw body, so a
// test can assert on the status code rather than only on success.
func do(t *testing.T, c *http.Client, method, url, body string) (int, string) {
	t.Helper()
	return send(t, c, nil, method, url, body)
}

func sendPreview(t *testing.T, c *http.Client, role, method, url, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = stringsReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LightShip-Preview-Role", role)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	response, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(response)
}

// doKey is the same, authenticated with a bearer API key instead of a cookie.
func doKey(t *testing.T, token, method, url, body string) (int, string) {
	t.Helper()
	return send(t, &http.Client{}, &token, method, url, body)
}

func send(t *testing.T, c *http.Client, token *string, method, url, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = stringsReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != nil {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func get(t *testing.T, c *http.Client, url string, into any) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func assertTraces(t *testing.T, who string, got listResult, want []string) {
	t.Helper()
	have := map[string]bool{}
	for _, tr := range got.Traces {
		have[tr.TraceID] = true
	}
	if len(have) != len(want) {
		ids := make([]string, 0, len(have))
		for id := range have {
			ids = append(ids, id)
		}
		t.Errorf("%s saw %v, want exactly %v", who, ids, want)
		return
	}
	for _, id := range want {
		if !have[id] {
			t.Errorf("%s should see %s but did not", who, id)
		}
	}
}

func stringsReader(s string) *stringReader { return &stringReader{s: s} }

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

// TestBootstrapCredential exercises the three EnsureAdmin branches end to end: generation on an
// empty database, the no-op (and above all no-reprint) restart, the first-login audit distinction,
// and env-hash rotation revoking the admin's sessions.
func TestBootstrapCredential(t *testing.T) {
	chDSN := os.Getenv("LIGHTSHIP_TEST_CLICKHOUSE")
	pgDSN := os.Getenv("DATABASE_URL")
	if chDSN == "" || pgDSN == "" {
		t.Skip("set DATABASE_URL and LIGHTSHIP_TEST_CLICKHOUSE to run")
	}
	ctx := context.Background()

	st, err := store.Open(ctx, pgDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Direct SQL, for state the API deliberately does not expose: the stored hash, session rows,
	// audit actions. The admin account is not mutable through the API, so the test resets it the
	// way break-glass does — in Postgres.
	pool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mustExec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	adminHash := func() string {
		t.Helper()
		var h *string
		if err := pool.QueryRow(ctx,
			`select password_hash from app_user where username = 'admin'`).Scan(&h); err != nil {
			t.Fatal(err)
		}
		if h == nil {
			return ""
		}
		return *h
	}
	auditCount := func(action string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`select count(*) from audit_log where username = 'admin' and action = $1`,
			action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sessionCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`select count(*) from session s join app_user u on u.id = s.user_id
			  where u.username = 'admin'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	mustChange := func() bool {
		t.Helper()
		var m bool
		if err := pool.QueryRow(ctx,
			`select must_change_password from app_user where username = 'admin'`).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	pending := func() bool {
		t.Helper()
		var p bool
		if err := pool.QueryRow(ctx,
			`select bootstrap_login_pending from app_user where username = 'admin'`).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// The test database is reused, so start from the empty-database shape.
	mustExec(`delete from app_user where username = 'admin'`)
	mustExec(`delete from audit_log where username = 'admin' and action in
		('auth.login', 'auth.bootstrap_credential_generated', 'auth.bootstrap_first_login')`)

	reader, err := traces.Open(chDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	models := model.New(st, time.Minute)
	srv := httptest.NewServer(httpapi.New(st, reader, models,
		slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()

	loginCode := func(password string) int {
		t.Helper()
		resp, err := (&http.Client{}).Post(srv.URL+"/login", "application/json",
			stringsReader(fmt.Sprintf(`{"username":"admin","password":%q}`, password)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	var generated string
	t.Run("an empty database generates a credential once", func(t *testing.T) {
		generated, err = st.EnsureAdmin(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if generated == "" {
			t.Fatal("expected a generated password on an empty database")
		}
		h := adminHash()
		if !strings.HasPrefix(h, "$argon2id$") {
			t.Fatalf("stored credential is not an argon2id hash: %q", h)
		}
		if strings.Contains(h, generated) {
			t.Fatal("the password itself must never be stored")
		}
		if err := auth.Verify(generated, h); err != nil {
			t.Fatalf("the stored hash does not match the printed password: %v", err)
		}
		if n := auditCount("auth.bootstrap_credential_generated"); n != 1 {
			t.Errorf("generation audit rows: got %d, want 1", n)
		}
		if !pending() {
			t.Error("generation must arm the first-login marker")
		}
		// The forced change is for a credential someone else generated and handed over. The admin
		// generated its own and nobody else read it, so there is no handover to complete — and a
		// flagged admin would be locked out of the setup it exists to perform.
		if mustChange() {
			t.Error("the bootstrap admin must not be flagged for a password change")
		}
	})

	t.Run("a restart is a no-op and never reprints", func(t *testing.T) {
		before := adminHash()
		again, err := st.EnsureAdmin(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if again != "" {
			t.Fatal("a restart reprinted a credential")
		}
		if adminHash() != before {
			t.Error("a restart must not rewrite the stored hash")
		}
		if n := auditCount("auth.bootstrap_credential_generated"); n != 1 {
			t.Errorf("generation audit rows after restart: got %d, want 1", n)
		}
	})

	t.Run("only the first login is audited as the bootstrap first login", func(t *testing.T) {
		signIn(t, srv.URL, "admin", generated)
		if n := auditCount("auth.bootstrap_first_login"); n != 1 {
			t.Fatalf("bootstrap first-login rows: got %d, want 1", n)
		}
		if n := auditCount("auth.login"); n != 0 {
			t.Errorf("the first login must not also be an ordinary auth.login (got %d)", n)
		}
		signIn(t, srv.URL, "admin", generated)
		if n := auditCount("auth.bootstrap_first_login"); n != 1 {
			t.Errorf("bootstrap first-login rows after a second login: got %d, want 1", n)
		}
		if n := auditCount("auth.login"); n != 1 {
			t.Errorf("ordinary auth.login rows: got %d, want 1", n)
		}
		if pending() {
			t.Error("the first login must consume the marker")
		}
	})

	rotated, err := auth.Hash("rotatedpw")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("an env hash that differs revokes the admin's sessions", func(t *testing.T) {
		admin := signIn(t, srv.URL, "admin", generated)
		if sessionCount() == 0 {
			t.Fatal("expected live admin sessions before rotation")
		}
		out, err := st.EnsureAdmin(ctx, rotated)
		if err != nil {
			t.Fatal(err)
		}
		if out != "" {
			t.Fatal("an env-managed boot must not generate a credential")
		}
		if n := sessionCount(); n != 0 {
			t.Errorf("rotation left %d admin sessions alive", n)
		}
		if code := status(t, admin, srv.URL+"/audit?limit=1"); code != http.StatusUnauthorized {
			t.Errorf("a revoked session read /audit: got %d, want 401", code)
		}
		if code := loginCode(generated); code != http.StatusUnauthorized {
			t.Errorf("the generated password survived rotation: got %d, want 401", code)
		}
		signIn(t, srv.URL, "admin", "rotatedpw")
		if n := auditCount("auth.bootstrap_first_login"); n != 1 {
			t.Error("a login after rotation must not count as the bootstrap first login")
		}
	})

	t.Run("the same env hash on the next boot does not revoke", func(t *testing.T) {
		admin := signIn(t, srv.URL, "admin", "rotatedpw")
		if _, err := st.EnsureAdmin(ctx, rotated); err != nil {
			t.Fatal(err)
		}
		if sessionCount() == 0 {
			t.Error("a boot with the unchanged env hash must not revoke sessions")
		}
		if code := status(t, admin, srv.URL+"/audit?limit=1"); code != http.StatusOK {
			t.Errorf("session after an unchanged-hash boot: got %d, want 200", code)
		}
	})

	t.Run("a UI password change survives an unchanged env hash", func(t *testing.T) {
		chosenHash, err := auth.Hash("chosen-in-the-ui")
		if err != nil {
			t.Fatal(err)
		}
		creds, err := st.Credentials(ctx, "admin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetOwnPassword(ctx, creds.ID, chosenHash, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.EnsureAdmin(ctx, rotated); err != nil {
			t.Fatal(err)
		}
		if code := loginCode("rotatedpw"); code != http.StatusUnauthorized {
			t.Errorf("the old environment password was restored: got %d, want 401", code)
		}
		if code := loginCode("chosen-in-the-ui"); code != http.StatusOK {
			t.Errorf("the UI-chosen password did not survive restart: got %d, want 200", code)
		}
	})
}
