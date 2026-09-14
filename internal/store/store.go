// Package store owns Postgres. It is the source of truth for the query path: a caller's roles and
// policies are resolved from these tables, never re-read from the config file.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lightshipHQ/lightship"
	"github.com/lightshipHQ/lightship/internal/auth"
	"github.com/lightshipHQ/lightship/internal/schema"
)

// The bootstrap admin is created from the environment, not through the API, so the `true` policy is
// never something an admin can hand out by accident.
const (
	adminUsername = "admin"
	// Guards Migrate against concurrent replicas. Arbitrary, but fixed: it identifies this
	// migration lock and nothing else in the database may reuse it.
	migrationLockKey = int64(7_263_918_441_002_517)
	adminRole        = "admin"
)

// AdminRole is exported so the query path can recognise the one role whose policy it compiles
// separately, rather than re-deriving the name from a string literal somewhere else.
const AdminRole = adminRole

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies pending .sql files in lexical order, each in its own transaction.
// Applying twice is a no-op.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	// Replicas boot together, so without a lock two of them race the same CREATE TABLE and one
	// loses on a duplicate-object error that reads like a corrupt deployment. The lock is held on
	// a dedicated connection for the whole migration and released when it returns, so the second
	// replica simply waits and then finds everything already applied. The key is arbitrary but
	// must never collide with another advisory lock in this database.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return nil, err
	}
	defer func() {
		// Best effort: a failed unlock is not worth masking the migration's own error, and the
		// lock is session-scoped so releasing the connection drops it regardless.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := s.pool.Exec(ctx, `create table if not exists schema_migrations (
		name text primary key, applied_at timestamptz not null default now())`); err != nil {
		return nil, err
	}
	done := map[string]bool{}
	rows, err := s.pool.Query(ctx, `select name from schema_migrations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		done[n] = true
	}
	rows.Close()

	entries, err := fs.Glob(lightship.Migrations, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)

	var applied []string
	for _, e := range entries {
		if done[e] {
			continue
		}
		body, err := fs.ReadFile(lightship.Migrations, e)
		if err != nil {
			return nil, err
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			tx.Rollback(ctx)
			return nil, fmt.Errorf("migration %s: %w", e, err)
		}
		if _, err := tx.Exec(ctx, `insert into schema_migrations (name) values ($1)`, e); err != nil {
			tx.Rollback(ctx)
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		applied = append(applied, e)
	}
	return applied, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ── sessions ────────────────────────────────────────────────────────────────

func TokenSHA(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateSession(ctx context.Context, userID, token string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`insert into session (token_sha, user_id, expires_at) values ($1, $2, $3)`,
		TokenSHA(token), userID, time.Now().Add(ttl))
	return err
}

// PruneSessions removes credentials that can no longer authenticate.
func (s *Store) PruneSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `delete from session where expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteSession ends one session. Sessions are rows rather than signed cookies precisely so that
// this is possible: a caller who signs out must stop being able to read, not merely stop being
// asked to sign in.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	tag, err := s.pool.Exec(ctx, `delete from session where token_sha = $1`, TokenSHA(token))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type Credentials struct {
	ID           string
	Username     string
	PasswordHash string
}

// ErrNoPassword is returned for a user that exists but has no password hash — an identity
// provisioned for SSO, or one whose password has been cleared. It is distinct from "no such user"
// so the login path can deny without logging an unusable-hash error on every attempt, and it must
// never be distinguishable to the caller.
var ErrNoPassword = errors.New("user has no password set")

func (s *Store) Credentials(ctx context.Context, username string) (*Credentials, error) {
	var c Credentials
	var hash *string
	err := s.pool.QueryRow(ctx,
		`select id, username, password_hash from app_user where username = $1`,
		username).Scan(&c.ID, &c.Username, &hash)
	if err != nil {
		return nil, err
	}
	if hash == nil {
		return nil, ErrNoPassword
	}
	c.PasswordHash = *hash
	return &c, nil
}

// EnsureAdmin creates the bootstrap admin and the role carrying the `true` policy. The policy is
// literal `true` rather than a special case in the query path: an admin bypass is a role like any
// other, so there is one enforcement mechanism rather than two.
//
// envHash is LIGHTSHIP_ADMIN_PASSWORD_HASH, and may be empty. Exactly one of three things happens
// to the credential:
//
//   - envHash set: a value different from the last applied environment hash is a rotation — the
//     recovery move for a leak — and the admin's sessions are revoked. An unchanged environment
//     hash does not overwrite a password the admin subsequently chose in the UI.
//   - envHash empty, a password already stored: the credential stays unchanged and any prior
//     environment tracker is cleared. A restart must never reprint a generated credential — a
//     reprint would re-emit it into every subsequent log shipment, converting a one-time exposure
//     into a recurring one. The stored hash is the marker; there is no flag row.
//   - neither: a random password is generated, only its argon2id hash is stored, and the password
//     is returned exactly once for the caller to print. The fact of generation is audited as
//     auth.bootstrap_credential_generated — never the value — and the first login on it will be
//     audited distinguishably (see ConsumeBootstrapFirstLogin).
func (s *Store) EnsureAdmin(ctx context.Context, envHash string) (generated string, err error) {
	if envHash != "" {
		if err := auth.ValidateHash(envHash); err != nil {
			return "", err
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := lockModelVersion(ctx, tx); err != nil {
		return "", err
	}

	var roleID string
	if err := tx.QueryRow(ctx,
		`insert into role (name) values ($1)
		 on conflict (name) do update set name = excluded.name returning id`,
		adminRole).Scan(&roleID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`insert into policy (role_id, title, description, expression)
		 values ($1, 'admin bypass', 'sees every row', 'true')
		 on conflict (role_id, title) do update set expression = excluded.expression`,
		roleID); err != nil {
		return "", err
	}

	// Which branch this boot takes depends on what is already stored.
	var userID string
	var storedHash *string
	var storedEnvHash *string
	err = tx.QueryRow(ctx,
		`select id, password_hash, admin_env_password_hash from app_user where username = $1`,
		adminUsername).Scan(&userID, &storedHash, &storedEnvHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	havePassword := err == nil && storedHash != nil && *storedHash != ""

	switch {
	case envHash != "":
		// The environment tracker changes only when its configured value changes. A self-service
		// password update changes password_hash without changing this tracker.
		envChanged := storedEnvHash == nil || *storedEnvHash != envHash
		rotating := envChanged && havePassword && *storedHash != envHash
		if envChanged {
			if err := tx.QueryRow(ctx,
				`insert into app_user (username, password_hash, admin_env_password_hash) values ($1, $2, $2)
			 on conflict (username) do update
			   set password_hash = excluded.password_hash,
			       admin_env_password_hash = excluded.admin_env_password_hash,
			       bootstrap_login_pending = false,
			       updated_at = now()
			 returning id`, adminUsername, envHash).Scan(&userID); err != nil {
				return "", err
			}
		}
		if rotating {
			if _, err := tx.Exec(ctx,
				`delete from session where user_id = $1`, userID); err != nil {
				return "", err
			}
		}
	case havePassword:
		// An absent environment value clears its tracker, so configuring a hash again later is a
		// deliberate rotation even when that same value was used in the past.
		if storedEnvHash != nil {
			if _, err := tx.Exec(ctx,
				`update app_user set admin_env_password_hash = null where id = $1`, userID); err != nil {
				return "", err
			}
		}
	default:
		// Empty database, or a hash cleared as break-glass. Generate.
		pw, err := GeneratePassword()
		if err != nil {
			return "", err
		}
		h, err := auth.Hash(pw)
		if err != nil {
			return "", err
		}
		if err := tx.QueryRow(ctx,
			`insert into app_user (username, password_hash, bootstrap_login_pending)
			 values ($1, $2, true)
			 on conflict (username) do update
			   set password_hash = excluded.password_hash, bootstrap_login_pending = true,
			       updated_at = now()
			 returning id`, adminUsername, h).Scan(&userID); err != nil {
			return "", err
		}
		generated = pw
	}

	if _, err := tx.Exec(ctx,
		`insert into role_assignment (user_id, role_id) values ($1, $2) on conflict do nothing`,
		userID, roleID); err != nil {
		return "", err
	}
	if err := bumpVersion(ctx, tx); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	if generated != "" {
		s.AuditWith(ctx, adminUsername, "auth.bootstrap_credential_generated", "", map[string]any{})
	}
	return generated, nil
}

// ── request-time identity ───────────────────────────────────────────────────

type Session struct {
	UserID   string
	Username string
	Roles    []string
	Attrs    map[string]string
	// Via names the API key a request arrived on, empty for a browser session. It exists so the
	// audit log can distinguish a person from the automation acting as them.
	Via string
	// MustChangePassword carries the enrollment flag to the middleware, which refuses every route
	// but the three a pending user needs. It is read with the session rather than looked up by the
	// handlers that care, because a route that forgets to ask would be a route the generated
	// password still works on.
	MustChangePassword bool
	// PreviewRole is set only for an administrator request that deliberately asks to be evaluated
	// as one role. It is request-scoped and never persisted with the session.
	PreviewRole string
}

// Lookup resolves a session token to the caller's roles and attributes. An expired session is not
// a session: deprovisioning must not wait for a cookie to lapse.
func (s *Store) Lookup(ctx context.Context, token string) (*Session, error) {
	var userID string
	err := s.pool.QueryRow(ctx,
		`select s.user_id
		   from session s join app_user u on u.id = s.user_id
		  where s.token_sha = $1 and s.expires_at > now()`,
		TokenSHA(token)).Scan(&userID)
	if err != nil {
		return nil, err
	}
	return s.loadSession(ctx, userID)
}

// loadSession reads roles and attributes fresh from Postgres. Both credential types resolve through
// it, so an API key and a browser session are subject to exactly the same policy resolution — a key
// is a way of authenticating, never a second authorization path.
func (s *Store) loadSession(ctx context.Context, userID string) (*Session, error) {
	sess := Session{UserID: userID, Attrs: map[string]string{}}
	if err := s.pool.QueryRow(ctx,
		`select username, must_change_password from app_user where id = $1`,
		userID).Scan(&sess.Username, &sess.MustChangePassword); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx,
		`select r.name from role_assignment ra join role r on r.id = ra.role_id
		  where ra.user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		sess.Roles = append(sess.Roles, n)
	}
	rows.Close()

	rows, err = s.pool.Query(ctx,
		`select key, value from user_attribute where user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		sess.Attrs[k] = v
	}
	return &sess, rows.Err()
}

func (s *Session) IsAdmin() bool {
	for _, r := range s.Roles {
		if r == adminRole {
			return true
		}
	}
	return false
}

// ── audit ───────────────────────────────────────────────────────────────────

// AuditDetail is written as jsonb. A list query records the filter and the result count, never
// every id returned — one broad listing would otherwise write thousands of rows.
func (s *Store) AuditWith(ctx context.Context, username, action, filters string, detail map[string]any) {
	body, err := json.Marshal(detail)
	if err != nil {
		body = []byte(`{}`)
	}
	// Never fatal to a request: losing an audit row must not deny a caller who was permitted.
	if _, err := s.pool.Exec(ctx,
		`insert into audit_log (username, action, policy_filters, detail)
		 values ($1, $2, $3, $4)`, username, action, nullable(filters), body); err != nil {
		slog.Error("audit write failed", "action", action, "username", username, "err", err)
	}
}

type AuditRow struct {
	At            time.Time      `json:"at"`
	Username      string         `json:"username"`
	Action        string         `json:"action"`
	PolicyFilters *string        `json:"policy_filters"`
	Detail        map[string]any `json:"detail"`
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditRow, error) {
	rows, err := s.pool.Query(ctx,
		`select at, username, action, policy_filters, detail
		   from audit_log order by at desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var r AuditRow
		var body []byte
		if err := rows.Scan(&r.At, &r.Username, &r.Action, &r.PolicyFilters, &body); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(body, &r.Detail)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneAudit enforces the configured retention. The log is not allowed to grow unbounded, and
// nobody is going to remember to run a cron.
func (s *Store) PruneAudit(ctx context.Context, days int) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`delete from audit_log where at < now() - make_interval(days => $1)`, days)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ── users ───────────────────────────────────────────────────────────────────

// Provisioning errors are typed rather than strings because the HTTP layer has to turn them into
// status codes, and a 404 that should have been a 409 is a bug an operator's script sees, not a
// person.
var (
	ErrNotFound    = errors.New("no such user")
	ErrExists      = errors.New("user already exists")
	ErrUnknownRole = errors.New("unknown role")
	ErrProtected   = errors.New("this is a bootstrap object and cannot be changed through the API")
)

type UserRecord struct {
	Username    string            `json:"username"`
	HasPassword bool              `json:"has_password"`
	Attributes  map[string]string `json:"attributes"`
	Roles       []string          `json:"roles"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// CreateUser provisions one user with their attributes and roles in a single transaction. A caller
// that half-succeeds would leave a user holding roles but not the attributes those roles' policies
// compare against, which reads as "sees nothing" and is indistinguishable from a policy bug.
//
// mustChangePassword arms the enrollment flag, which is what makes a handed-over password a
// handover rather than a shared secret: until the user replaces it, every route but GET /me,
// POST /me/password and POST /logout refuses them. It is false for the identity nobody signs into
// interactively, whose password exists only to mint one API key.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string,
	attrs map[string]string, roles []string, mustChangePassword bool) error {
	if username == adminUsername {
		return ErrProtected
	}
	if passwordHash != "" {
		if err := auth.ValidateHash(passwordHash); err != nil {
			return err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var uid string
	err = tx.QueryRow(ctx,
		`insert into app_user (username, password_hash, must_change_password)
		 values ($1, $2, $3)
		 on conflict (username) do nothing
		 returning id`, username, nullable(passwordHash), mustChangePassword).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	if err := setAttrs(ctx, tx, uid, attrs, SourceAPI); err != nil {
		return err
	}
	if err := setRoles(ctx, tx, uid, roles); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// userSelect aggregates roles and attributes in the query rather than in three round trips per
// user. Listing is now unbounded in a way the config file never was, so the shape of this read is
// the difference between a page and a stampede.
const userSelect = `
	select u.username,
	       u.password_hash is not null,
	       u.created_at, u.updated_at,
	       coalesce((select array_agg(r.name order by r.name)
	                   from role_assignment ra join role r on r.id = ra.role_id
	                  where ra.user_id = u.id), '{}'),
	       coalesce((select jsonb_object_agg(a.key, a.value)
	                   from user_attribute a where a.user_id = u.id), '{}'::jsonb)
	  from app_user u`

func scanUser(row pgx.Row) (*UserRecord, error) {
	var u UserRecord
	var attrs []byte
	if err := row.Scan(&u.Username, &u.HasPassword, &u.CreatedAt, &u.UpdatedAt,
		&u.Roles, &attrs); err != nil {
		return nil, err
	}
	u.Attributes = map[string]string{}
	_ = json.Unmarshal(attrs, &u.Attributes)
	return &u, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]UserRecord, error) {
	rows, err := s.pool.Query(ctx, userSelect+` order by u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserRecord{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Store) GetUser(ctx context.Context, username string) (*UserRecord, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, userSelect+` where u.username = $1`, username))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// SourceAPI marks an attribute set through the provisioning API — the only source there is today.
// The column exists so that when IdP-supplied values arrive, precedence between them and a hand-set
// one is a stored fact rather than a guess (migrations/0001_init.sql).
const SourceAPI = "api"

// PatchAttributes updates named attributes and leaves the rest alone. A nil value removes the
// attribute, which is what keeps a grant narrowable — an attribute that could only ever be set
// would be a privilege nobody could take back. This is JSON Merge Patch semantics (RFC 7386), so
// `{"tenant_id": null}` is a removal and an absent key is "no opinion".
//
// It returns the complete resulting set, because what an operator needs in the audit log is the
// grant the user now holds, not the delta that produced it.
func (s *Store) PatchAttributes(ctx context.Context, username string,
	attrs map[string]*string) (map[string]string, error) {
	if username == adminUsername {
		return nil, ErrProtected
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	uid, err := userID(ctx, tx, username)
	if err != nil {
		return nil, err
	}
	set := map[string]string{}
	for k, v := range attrs {
		if v == nil {
			if _, err := tx.Exec(ctx,
				`delete from user_attribute where user_id = $1 and key = $2`, uid, k); err != nil {
				return nil, fmt.Errorf("attribute %q: %w", k, err)
			}
			continue
		}
		set[k] = *v
	}
	if err := setAttrs(ctx, tx, uid, set, SourceAPI); err != nil {
		return nil, err
	}

	after := map[string]string{}
	rows, err := tx.Query(ctx, `select key, value from user_attribute where user_id = $1`, uid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return nil, err
		}
		after[k] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return after, tx.Commit(ctx)
}

// UpdateUser sets the password hash and roles, and reports how many sessions it revoked.
//
// A nil field is left alone; an empty roles slice removes every role, which is the supported way to
// suspend someone without deleting their audit history. Roles need no revocation of their own — the
// session middleware re-reads them on every request, so a role removed is a role gone on the
// caller's next call.
//
// A password change is different, and is the one mutation that would otherwise not take effect. It
// revokes every session the user holds, because the reason to reset a password is usually that
// someone else knows it, and leaving their cookie working for the rest of the TTL would answer the
// wrong half of the problem.
//
// API keys are not revoked: a key is a separate credential with its own revocation
// (DELETE /keys/{id}). A non-empty administrator-supplied password re-arms enrollment, which
// gates both credential types until the owner replaces the password.
func (s *Store) UpdateUser(ctx context.Context, username string, passwordHash *string,
	roles *[]string) (revoked int64, err error) {
	if username == adminUsername {
		return 0, ErrProtected
	}
	if passwordHash != nil && *passwordHash != "" {
		if err := auth.ValidateHash(*passwordHash); err != nil {
			return 0, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	uid, err := userID(ctx, tx, username)
	if err != nil {
		return 0, err
	}
	if passwordHash != nil {
		if _, err := tx.Exec(ctx,
			`update app_user
			    set password_hash = $2,
			        must_change_password = case when $2::text is not null then true else must_change_password end,
			        updated_at = now()
			  where id = $1`,
			uid, nullable(*passwordHash)); err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `delete from session where user_id = $1`, uid)
		if err != nil {
			return 0, err
		}
		revoked = tag.RowsAffected()
	}
	if roles != nil {
		if _, err := tx.Exec(ctx, `delete from role_assignment where user_id = $1`, uid); err != nil {
			return 0, err
		}
		if err := setRoles(ctx, tx, uid, *roles); err != nil {
			return 0, err
		}
	}
	return revoked, tx.Commit(ctx)
}

// SetOwnPassword completes a self-service password change: it stores the new hash, clears the
// enrollment flag, and revokes every session the user holds except the one making the call.
//
// Revoking the others is the point of doing this in one transaction rather than in the handler. A
// password is usually changed because the old one is no longer trusted — it was read off a screen
// and handed over, or it leaked — and a change that leaves the older sessions alive answers only
// half of that. The caller's own session survives because signing a person out of the page they
// just used reads as a failure, and they hold the new password anyway.
//
// keepToken is the caller's session token, empty for a caller authenticated with an API key, in
// which case no session is spared. API keys are untouched here for the same reason a password
// reset does not revoke them: a key is a separate credential with its own revocation.
func (s *Store) SetOwnPassword(ctx context.Context, userID, passwordHash, keepToken string) (revoked int64, err error) {
	if err := auth.ValidateHash(passwordHash); err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`update app_user
		    set password_hash = $2, must_change_password = false, updated_at = now()
		  where id = $1`, userID, passwordHash)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}
	keep := ""
	if keepToken != "" {
		keep = TokenSHA(keepToken)
	}
	tag, err = tx.Exec(ctx,
		`delete from session where user_id = $1 and token_sha <> $2`, userID, keep)
	if err != nil {
		return 0, err
	}
	revoked = tag.RowsAffected()
	return revoked, tx.Commit(ctx)
}

// DeleteUser is the deprovisioning path. Sessions and API keys cascade with the row, so access ends
// at the moment of the call rather than whenever a cookie or token would have lapsed. The audit log
// does not cascade — it denormalises the username precisely so a deleted user cannot take their
// query history with them.
func (s *Store) DeleteUser(ctx context.Context, username string) error {
	if username == adminUsername {
		return ErrProtected
	}
	tag, err := s.pool.Exec(ctx, `delete from app_user where username = $1`, username)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func userID(ctx context.Context, tx pgx.Tx, username string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `select id from app_user where username = $1`, username).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

func setAttrs(ctx context.Context, tx pgx.Tx, uid string, attrs map[string]string, source string) error {
	addedKey := false
	for k, v := range attrs {
		if !schema.ValidIdent(k) {
			return fmt.Errorf("attribute %q is not a usable policy identifier", k)
		}
		tag, err := tx.Exec(ctx,
			`insert into user_attribute_key (name) values ($1) on conflict do nothing`, k)
		if err != nil {
			return fmt.Errorf("attribute %q: %w", k, err)
		}
		addedKey = addedKey || tag.RowsAffected() > 0
		if _, err := tx.Exec(ctx,
			`insert into user_attribute (user_id, key, value, source) values ($1, $2, $3, $4)
			 on conflict (user_id, key) do update
			   set value = excluded.value, source = excluded.source`,
			uid, k, v, source); err != nil {
			return fmt.Errorf("attribute %q: %w", k, err)
		}
	}
	if addedKey {
		if err := bumpVersion(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

// setRoles resolves names to ids explicitly so that an unknown role is rejected rather than
// silently skipped. A role assignment that quietly does not happen is a caller who sees nothing and
// no indication why.
func setRoles(ctx context.Context, tx pgx.Tx, uid string, roles []string) error {
	for _, rn := range roles {
		var rid string
		err := tx.QueryRow(ctx, `select id from role where name = $1`, rn).Scan(&rid)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %q", ErrUnknownRole, rn)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`insert into role_assignment (user_id, role_id) values ($1, $2) on conflict do nothing`,
			uid, rid); err != nil {
			return err
		}
	}
	return nil
}

// ── api keys ────────────────────────────────────────────────────────────────

// A key is a credential, not an authorization mechanism. It resolves through loadSession to the
// same roles and attributes a browser session would, so the guarantee is unchanged: a key cannot
// return a row its principal's roles do not permit.
//
// Every key belongs to a user. An unattended provisioner — a script, CI, a Terraform provider — is
// an ordinary user holding the admin role that nobody signs into interactively, which gives it
// attributes, scoping, revocation and audit attribution with no second identity concept beside the
// one the guarantee rests on. A user-less key would need each of those reinvented.
type KeyRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Username   string     `json:"username"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// CreateKey mints a key that acts as `username`. It stores no roles: LookupKey resolves them from
// the user on every request, so the key tracks role and attribute changes live and can never hold a
// grant the user has since lost. Only the token's sha256 is stored, and the token is returned to
// the caller once.
//
// The bootstrap admin may mint one, unlike every other write that names it. The ErrProtected
// guards elsewhere keep the account itself — password, roles, existence — out of the API's reach;
// a key merely acts as the account, it does not change it, and first-run setup depends on the only
// user that exists being able to hand a credential to an agent.
func (s *Store) CreateKey(ctx context.Context, name, token, username string,
	expiresAt *time.Time) (string, error) {
	var uid string
	err := s.pool.QueryRow(ctx, `select id from app_user where username = $1`, username).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var id string
	err = s.pool.QueryRow(ctx,
		`insert into api_key (name, token_sha, user_id, created_by, expires_at)
		 values ($1, $2, $3, $4, $5) returning id`,
		name, TokenSHA(token), uid, username, expiresAt).Scan(&id)
	return id, err
}

// ListKeys returns every key, or only one user's when forUser is set. A caller who may create keys
// for themselves must be able to see and revoke them; they must not be able to enumerate anyone
// else's, which would be a map of the deployment's automation.
func (s *Store) ListKeys(ctx context.Context, forUser string) ([]KeyRecord, error) {
	rows, err := s.pool.Query(ctx,
		`select k.id, k.name, u.username, k.created_by, k.created_at,
		        k.expires_at, k.last_used_at, k.revoked_at
		   from api_key k join app_user u on u.id = k.user_id
		  where $1 = '' or u.username = $1
		  order by k.created_at desc`, forUser)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyRecord{}
	for rows.Next() {
		var k KeyRecord
		if err := rows.Scan(&k.ID, &k.Name, &k.Username, &k.CreatedBy, &k.CreatedAt,
			&k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey is a tombstone rather than a delete, so a key that appears in the audit log can still
// be named after it stops working.
// RevokeKey tombstones a key. When ownedBy is set the key must belong to that user, and a key that
// does not is reported as absent rather than forbidden: whether someone else's key exists is not a
// fact a non-admin is entitled to learn from a status code.
func (s *Store) RevokeKey(ctx context.Context, id, ownedBy string) error {
	tag, err := s.pool.Exec(ctx,
		`update api_key set revoked_at = now()
		  where id = $1 and revoked_at is null
		    and ($2 = '' or user_id = (select id from app_user where username = $2))`, id, ownedBy)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LookupKey resolves a bearer token. A revoked or expired key is not a key: revocation must take
// effect at the moment of the call, not whenever something happens to be re-read.
func (s *Store) LookupKey(ctx context.Context, token string) (*Session, error) {
	var id, name, uid string
	err := s.pool.QueryRow(ctx,
		`select id, name, user_id from api_key
		  where token_sha = $1 and revoked_at is null
		    and (expires_at is null or expires_at > now())`,
		TokenSHA(token)).Scan(&id, &name, &uid)
	if err != nil {
		return nil, err
	}
	s.touchKey(ctx, id)

	sess, err := s.loadSession(ctx, uid)
	if err != nil {
		return nil, err
	}
	sess.Via = name
	return sess, nil
}

// touchKey records last use at most once a minute. An unbounded key is likely to be serving reads
// at volume, and a write per read would make the audit convenience cost more than it is worth.
func (s *Store) touchKey(ctx context.Context, id string) {
	if _, err := s.pool.Exec(ctx,
		`update api_key set last_used_at = now()
		  where id = $1 and (last_used_at is null or last_used_at < now() - interval '1 minute')`,
		id); err != nil {
		slog.Error("api key last_used update failed", "err", err)
	}
}

// ── the access model ────────────────────────────────────────────────────────

// LoadModel reads the whole access model in one transaction. A request must never be served by a
// mix of versions — fields from before a write and policies from after would be a policy compiled
// against a vocabulary that no longer matches.
func (s *Store) LoadModel(ctx context.Context) (schema.Model, error) {
	var m schema.Model
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return m, err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx,
		`select table_name, trace_id, ts, span_id, parent_span_id, span_name
		   from source_binding where only_row`).Scan(
		&m.Binding.Table, &m.Binding.TraceID, &m.Binding.Timestamp, &m.Binding.SpanID,
		&m.Binding.ParentSpanID, &m.Binding.Name)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return m, err
	}
	// No binding yet is not an error: a fresh deployment boots, serves /healthz and accepts the
	// setup calls that will create one. Queries fail closed until it exists.

	rows, err := tx.Query(ctx,
		`select map_column, name, logical_type, filterable, policy_ref from field
		  order by map_column, name`)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var f schema.Field
		if err := rows.Scan(&f.Map, &f.Name, &f.LogicalType, &f.Filterable, &f.Policy); err != nil {
			rows.Close()
			return m, err
		}
		m.Fields = append(m.Fields, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return m, err
	}

	rows, err = tx.Query(ctx, `select name from user_attribute_key order by name`)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return m, err
		}
		m.UserAttrs = append(m.UserAttrs, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return m, err
	}

	rows, err = tx.Query(ctx,
		`select r.name, p.title, coalesce(p.description, ''), p.expression
		   from role r left join policy p on p.role_id = r.id
		  order by r.name, p.title`)
	if err != nil {
		return m, err
	}
	byName := map[string]int{}
	for rows.Next() {
		var name string
		var title, desc, expr *string
		if err := rows.Scan(&name, &title, &desc, &expr); err != nil {
			rows.Close()
			return m, err
		}
		i, ok := byName[name]
		if !ok {
			m.Roles = append(m.Roles, schema.Role{Name: name})
			i = len(m.Roles) - 1
			byName[name] = i
		}
		if title != nil && expr != nil {
			m.Roles[i].Policies = append(m.Roles[i].Policies, schema.Policy{
				Title: *title, Description: derefOr(desc), Expression: *expr})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return m, err
	}

	if err := tx.QueryRow(ctx, `select version from model_version where only_row`).
		Scan(&m.Version); err != nil {
		return m, err
	}
	return m, tx.Commit(ctx)
}

func derefOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ErrModelConflict means the model changed after the caller read and validated its proposal.
var ErrModelConflict = errors.New("access model changed; reload and retry the change")

// lockModelVersion serializes model writers before they touch any model or assignment rows.
func lockModelVersion(ctx context.Context, tx pgx.Tx) (int64, error) {
	var version int64
	err := tx.QueryRow(ctx, `select version from model_version where only_row for update`).Scan(&version)
	return version, err
}

func checkModelVersion(ctx context.Context, tx pgx.Tx, expected int64) error {
	version, err := lockModelVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version != expected {
		return ErrModelConflict
	}
	return nil
}

// bumpVersion is called inside every model mutation after locking model_version. It makes the
// committed change distinguishable from the previous compiled snapshot.
func bumpVersion(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `update model_version set version = version + 1 where only_row`)
	return err
}

// SetBinding replaces the source binding. Validation happens above this: by the time a binding is
// written it has already been compiled against, so an unusable one cannot be stored.
func (s *Store) SetBinding(ctx context.Context, b schema.Binding, expectedVersion int64) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := checkModelVersion(ctx, tx, expectedVersion); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`insert into source_binding (only_row, table_name, trace_id, ts, span_id, parent_span_id,
		                             span_name)
		 values (true, $1, $2, $3, $4, $5, $6)
		 on conflict (only_row) do update set
		   table_name = excluded.table_name, trace_id = excluded.trace_id, ts = excluded.ts,
		   span_id = excluded.span_id, parent_span_id = excluded.parent_span_id,
		   span_name = excluded.span_name, updated_at = now()`,
		b.Table, b.TraceID, b.Timestamp, b.SpanID, b.ParentSpanID, b.Name); err != nil {
		return err
	}
	if err := bumpVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PutFields replaces the whole trace-field set. User attribute keys are registered when their first
// value is stored and remain available so a field edit cannot invalidate an existing policy.
func (s *Store) PutFields(ctx context.Context, fields []schema.Field, expectedVersion int64) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := checkModelVersion(ctx, tx, expectedVersion); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `delete from field`); err != nil {
		return err
	}
	for _, f := range fields {
		if _, err := tx.Exec(ctx,
			`insert into field (map_column, name, logical_type, filterable, policy_ref) values ($1, $2, $3, $4, $5)`,
			f.Map, f.Name, f.Type(), f.Filterable, f.Policy); err != nil {
			return fmt.Errorf("field %s: %w", f.Ref(), err)
		}
	}
	if err := bumpVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PutRole creates or replaces one role and its policies.
func (s *Store) PutRole(ctx context.Context, r schema.Role, expectedVersion int64) error {
	// The admin role carries the `true` policy and is created from the environment by EnsureAdmin.
	// DELETE already refused it; so must PUT, or the two guards disagree about the same row.
	if r.Name == adminRole {
		return ErrProtected
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := checkModelVersion(ctx, tx, expectedVersion); err != nil {
		return err
	}

	var id string
	if err := tx.QueryRow(ctx,
		`insert into role (name) values ($1)
		 on conflict (name) do update set name = excluded.name returning id`, r.Name).Scan(&id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from policy where role_id = $1`, id); err != nil {
		return err
	}
	for _, p := range r.Policies {
		if _, err := tx.Exec(ctx,
			`insert into policy (role_id, title, description, expression) values ($1, $2, $3, $4)`,
			id, p.Title, nullable(p.Description), p.Expression); err != nil {
			return fmt.Errorf("policy %q: %w", p.Title, err)
		}
	}
	if err := bumpVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteRole removes a role and, by cascade, its policies and every assignment of it. Callers who
// held it lose it on their next request, because the session middleware re-reads roles every time.
func (s *Store) DeleteRole(ctx context.Context, name string, expectedVersion int64) error {
	if name == adminRole {
		return ErrProtected
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := checkModelVersion(ctx, tx, expectedVersion); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `delete from role where name = $1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := bumpVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
