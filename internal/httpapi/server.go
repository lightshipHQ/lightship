// Package httpapi is the HTTP surface. Days 1–2 carry health and login only; the trace endpoints
// arrive with the translator, because there is no safe way to serve a trace before policies can
// be turned into a WHERE clause.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lightshipHQ/lightship/internal/auth"
	"github.com/lightshipHQ/lightship/internal/model"
	"github.com/lightshipHQ/lightship/internal/store"
	"github.com/lightshipHQ/lightship/internal/traces"
)

const sessionTTL = 12 * time.Hour
const previewRoleHeader = "X-LightShip-Preview-Role"

type Server struct {
	st     *store.Store
	reader *traces.Reader
	// models serves the compiled access model. It is read per request rather than captured at
	// startup, because the model is editable through this API now.
	models *model.Cache
	log    *slog.Logger
	// mux is this server's own router, used to dispatch admin MCP tool calls back through the REST
	// handlers. See mcpadmin.go.
	mux       *http.ServeMux
	demo      DemoConfig
	limits    ResourceLimits
	keyExpiry keyExpiryPolicy
	loginGate chan struct{}
	queryGate chan struct{}
}

type Option func(*Server)

type ResourceLimits struct {
	LoginConcurrency int
	QueryConcurrency int
	QueryTimeout     time.Duration
	MaxQueryWindow   time.Duration
}

func WithResourceLimits(limits ResourceLimits) Option {
	return func(s *Server) {
		s.limits = limits
		if limits.LoginConcurrency > 0 {
			s.loginGate = make(chan struct{}, limits.LoginConcurrency)
		}
		if limits.QueryConcurrency > 0 {
			s.queryGate = make(chan struct{}, limits.QueryConcurrency)
		}
	}
}

func New(
	st *store.Store, reader *traces.Reader, models *model.Cache, log *slog.Logger, options ...Option,
) http.Handler {
	s := &Server{st: st, reader: reader, models: models, log: log}
	for _, option := range options {
		option(s)
	}
	mux := http.NewServeMux()
	// The admin MCP tools dispatch through this mux rather than re-implementing the handlers, so
	// there is one implementation of every operation and MCP differs only in protocol.
	s.mux = mux
	mux.HandleFunc("GET /", s.ui)
	mux.HandleFunc("GET /ui/{name}", s.uiAsset)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /login", s.login)
	if s.demo.enabled() {
		mux.HandleFunc("GET /demo", s.demoStatus)
	}
	mux.Handle("POST /logout", s.whilePasswordPending(s.logout))
	// Who am I, and what may I do. It exists so a client learns its own privileges by asking rather
	// than by attempting something and reading the refusal, and it is one of the three routes a
	// caller pending a password change may still reach.
	mux.Handle("GET /me", s.whilePasswordPending(s.me))
	mux.Handle("POST /me/password", s.whilePasswordPending(s.demoPasswordLocked(s.changePassword)))
	// One read, and it returns rows. A filter does not belong in a URL, and bulk rows do not
	// belong in a context — so this is a POST that a client streams to a file.
	mux.Handle("POST /traces/query", s.authenticated(s.queryLimited(s.queryTraces)))
	mux.Handle("GET /traces/{id}", s.authenticated(s.queryLimited(s.traceDetail)))
	mux.Handle("GET /audit", s.adminOnly(s.listAudit))

	// Provisioning. Admin-only, and audited on every mutation.
	mux.Handle("POST /users", s.adminOnly(s.createUser))
	mux.Handle("GET /users", s.adminOnly(s.listUsers))
	mux.Handle("GET /users/{username}", s.adminOnly(s.getUser))
	mux.Handle("PATCH /users/{username}", s.adminOnly(s.updateUser))
	mux.Handle("DELETE /users/{username}", s.adminOnly(s.deleteUser))
	mux.Handle("PATCH /users/{username}/attributes", s.adminOnly(s.patchAttributes))
	// Keys are not admin-only: the API and MCP are the primary way this is consumed, so a caller
	// has to be able to mint a credential for themselves. Each handler scopes what a non-admin may
	// name, see and revoke.
	mux.Handle("POST /keys", s.authenticated(s.createKey))
	mux.Handle("GET /keys", s.authenticated(s.listKeys))
	mux.Handle("DELETE /keys/{id}", s.authenticated(s.revokeKey))

	// The access model. Admin-only and audited, and every write compiles before it is stored.
	mux.Handle("POST /schema/discover", s.adminOnly(s.discover))
	mux.Handle("GET /schema", s.adminOnly(s.getSchema))
	mux.Handle("GET /schema/optimizations", s.adminOnly(s.optimizations))
	mux.Handle("PUT /schema/binding", s.adminOnly(s.putBinding))
	mux.Handle("PUT /schema/fields", s.adminOnly(s.putFields))
	mux.Handle("PUT /roles/{name}", s.adminOnly(s.putRole))
	mux.Handle("DELETE /roles/{name}", s.adminOnly(s.deleteRole))
	// Filterable fields are what a caller needs to build a query, so this one is not admin-only.
	// Which fields carry the security model is not something every caller needs to know.
	mux.Handle("GET /filter/schema", s.authenticated(s.filterSchema))

	// MCP has two deliberately separate surfaces. The everyday endpoint never offers or dispatches
	// setup tools, even to an administrator. Setup is opt-in and the whole endpoint is admin-only;
	// the REST handlers it dispatches to repeat that authorization check.
	// A GET probe must learn "exists, wants POST" (405), never 404 — a client would conclude the
	// endpoint is missing. The mux would answer 405 itself, but the GET / catch-all fully matches
	// this path and swallows the probe with the UI's 404, so the route is explicit. See mcpGet.
	mux.HandleFunc("GET /mcp", mcpGet)
	mux.Handle("POST /mcp", s.authenticated(s.queryLimited(s.analysisMCP)))
	mux.HandleFunc("GET /mcp/setup", mcpGet)
	mux.Handle("POST /mcp/setup", s.adminOnly(s.setupMCP))
	return mux
}

type ctxKey int

const sessionKey ctxKey = 0

func sessionFrom(ctx context.Context) *store.Session {
	s, _ := ctx.Value(sessionKey).(*store.Session)
	return s
}

// authenticated resolves the caller on every request rather than trusting the credential's
// contents. Roles and attributes are read fresh, so a role removed from a caller takes effect on
// their next request rather than when their session happens to expire.
//
// Two credentials reach the same place: a browser session cookie, and a bearer API key for the
// unattended callers — provisioning scripts, CI, collectors — that have no browser to hold a
// cookie. Both resolve through the store to the same roles and attributes, so a key is a way of
// authenticating and never a second authorization path.
func (s *Server) authenticated(next http.HandlerFunc) http.Handler {
	return s.resolve(next, false)
}

// whilePasswordPending is authenticated for the three routes a caller pending a password change may
// still reach: reading their own status, changing the password, and signing out. Every other route
// is refused until the flag clears, so a credential an admin generated and read off a screen stops
// working the moment it has been used once.
func (s *Server) whilePasswordPending(next http.HandlerFunc) http.Handler {
	return s.resolve(next, true)
}

// resolve authenticates and, unless the route is one of the enrollment exceptions, enforces the
// pending-password-change flag. The check lives here rather than in the handlers because a handler
// that forgot it would be a handler the superseded password still works on — the refusal has to be
// a property of being authenticated, not something each route remembers to ask for.
func (s *Server) resolve(next http.HandlerFunc, allowPending bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deny := func() {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not signed in"})
		}
		var sess *store.Session
		var err error
		if tok, ok := bearer(r); ok {
			sess, err = s.st.LookupKey(r.Context(), tok)
		} else if c, cerr := r.Cookie("lightship_session"); cerr == nil {
			sess, err = s.st.Lookup(r.Context(), c.Value)
		} else {
			deny()
			return
		}
		if err != nil {
			deny()
			return
		}
		// Role preview is an admin-only, request-scoped impersonation. The actor remains the same
		// user for audit, while authorization and the interface are evaluated with only this role.
		if role := strings.TrimSpace(r.Header.Get(previewRoleHeader)); role != "" {
			if !sess.IsAdmin() {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin only"})
				return
			}
			if len(role) > 128 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "role name is too long"})
				return
			}
			stored, loadErr := s.st.LoadModel(r.Context())
			if loadErr != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "roles unavailable"})
				return
			}
			known := false
			for _, candidate := range stored.Roles {
				if candidate.Name == role {
					known = true
					break
				}
			}
			if !known {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown role"})
				return
			}
			preview := *sess
			preview.Roles = []string{role}
			preview.PreviewRole = role
			sess = &preview
		}
		if sess.MustChangePassword && !allowPending {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":                "password change required — POST /me/password",
				"must_change_password": true})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

// adminOnly guards the provisioning surface. Admin is a role like any other, resolved the same way
// as every other role, so there is one place that decides what a caller holds.
func (s *Server) adminOnly(next http.HandlerFunc) http.Handler {
	return s.authenticated(func(w http.ResponseWriter, r *http.Request) {
		if !sessionFrom(r.Context()).IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin only"})
			return
		}
		next(w, r)
	})
}

// bearer reports an Authorization header carrying something shaped like an API key. Anything else
// falls through to the cookie, so a stray header cannot turn a valid browser session into a 401.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || !auth.LooksLikeKey(tok) {
		return "", false
	}
	return tok, true
}

// audit is the one way a request writes an audit row. Every call site goes through it so that the
// credential a request arrived on cannot be recorded by some handlers and forgotten by others — a
// key-driven read attributed to a bare username is an audit trail that names the wrong actor.
func (s *Server) audit(r *http.Request, action, filters string, detail map[string]any) {
	sess := sessionFrom(r.Context())
	if sess.Via != "" {
		detail["via_key"] = sess.Via
	}
	if sess.PreviewRole != "" {
		detail["preview_role"] = sess.PreviewRole
	}
	s.st.AuditWith(r.Context(), sess.Username, action, filters, detail)
}

// health reports on Postgres only. ClickHouse being unreachable must not stop the control plane
// from starting or answering: an unreachable source denies queries, which is the fail-closed
// behaviour we want, and refusing to boot would instead take the audit log down with it.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.st.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "postgres": "down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request"})
		return
	}
	if !acquireGate(w, s.loginGate) {
		return
	}
	if s.loginGate != nil {
		defer func() { <-s.loginGate }()
	}

	// One message and one code for every failure below: an unknown username and a wrong password
	// must be indistinguishable to a caller enumerating accounts.
	deny := func() {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid credentials"})
	}

	cred, err := s.st.Credentials(r.Context(), body.Username)
	if err != nil {
		// Still spend the work of a verify so the response time does not reveal whether the
		// username exists — or, for ErrNoPassword, whether it exists without a password. An
		// identity provisioned ahead of SSO must not be enumerable through the password endpoint.
		_ = auth.Verify(body.Password, dummyHash)
		deny()
		return
	}
	if err := auth.Verify(body.Password, cred.PasswordHash); err != nil {
		if !errors.Is(err, auth.ErrMismatch) {
			s.log.Error("stored password hash is unusable", "username", cred.Username, "err", err)
		}
		deny()
		return
	}

	token, err := newToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not start session"})
		return
	}
	if err := s.st.CreateSession(r.Context(), cred.ID, token, sessionTTL); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not start session"})
		return
	}
	// The first login on a generated bootstrap credential is recorded under its own action, because
	// that password was printed once into a log stream a collector may ship elsewhere — whether
	// anyone else used the printed line should be answerable by query, not inference. Every login
	// after the first, and every login on an env-managed credential, is an ordinary auth.login.
	action := "auth.login"
	first, err := s.st.ConsumeBootstrapFirstLogin(r.Context(), cred.ID)
	if err != nil {
		s.log.Error("bootstrap first-login check failed", "username", cred.Username, "err", err)
	}
	if first {
		action = "auth.bootstrap_first_login"
	}
	s.st.AuditWith(r.Context(), cred.Username, action, "", map[string]any{})

	http.SetCookie(w, &http.Cookie{
		Name:     "lightship_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   overTLS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
	writeJSON(w, http.StatusOK, map[string]any{"username": cred.Username})
}

// overTLS reports whether the request reached the client over TLS, which decides the session
// cookie's Secure flag. r.TLS alone is wrong for the deployment this product recommends: a
// TLS-terminating proxy speaks plain HTTP to the container, so r.TLS is nil on every request and
// the session cookie would go out without Secure — free to be replayed over plaintext. The
// forwarded header is only trustworthy because it is set by that same proxy; a deployment that
// exposes the control plane directly to the internet without one has a larger problem than this.
func overTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// logout deletes the session row and clears the cookie. Deleting the row is the part that matters:
// clearing a cookie only stops the browser presenting a credential that would still have worked.
//
// An API key is not a session and cannot be logged out of — revoke it instead, which is a different
// operation with a different audit trail and a different blast radius.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	if sess.Via != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "an API key is not a session; revoke it with DELETE /keys/{id}"})
		return
	}
	c, err := r.Cookie("lightship_session")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "not a cookie session"})
		return
	}
	// A session already gone is a successful logout, not an error: the caller asked to be signed
	// out and they are.
	if err := s.st.DeleteSession(r.Context(), c.Value); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not sign out"})
		return
	}
	s.audit(r, "auth.logout", "", map[string]any{})

	http.SetCookie(w, &http.Cookie{
		Name:     "lightship_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   overTLS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"signed_out": true})
}

// me answers "who am I and what may I do". A client that has to learn its own privileges by
// attempting an admin route and reading the refusal spends a request per page load to be told no,
// and writes an error into the caller's console for a state that is not an error.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"username":             sess.Username,
		"is_admin":             sess.IsAdmin(),
		"actor_is_admin":       sess.IsAdmin() || sess.PreviewRole != "",
		"preview_role":         sess.PreviewRole,
		"roles":                sess.Roles,
		"must_change_password": sess.MustChangePassword,
		"is_demo":              s.demo.enabled() && sess.Username == s.demo.Username,
	})
}

// changePassword is the self-service password change, and the one place in the product where a
// plaintext password crosses this boundary inbound.
//
// The rule it bends exists so that an admin never handles another user's plaintext: a hash is all
// POST /users and PATCH /users/{u} will accept, precisely because an admin who typed the password
// would know it forever and could then sign in as that person with nothing in the audit log to
// distinguish it. That reasoning does not apply to the account's own owner, who knows the password
// already — and a browser cannot compute argon2id, so requiring a hash here would mean either no
// self-service change at all or shipping the whole enrollment through the admin again.
//
// It therefore acts only on the authenticated caller. There is no username parameter and no admin
// variant, so this route can never be pointed at someone else, and knowing the current password is
// required besides — a stolen session cannot be used to lock its owner out.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.NewPassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "new_password is required"})
		return
	}
	// A floor rather than a policy. The password this most often replaces was generated with
	// ~119 bits of entropy, and a replacement short enough to be guessed offline would quietly
	// undo that.
	if len([]rune(body.NewPassword)) < minPasswordLen {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("new_password must be at least %d characters", minPasswordLen)})
		return
	}

	sess := sessionFrom(r.Context())
	cred, err := s.st.Credentials(r.Context(), sess.Username)
	if err != nil {
		// The caller authenticated a moment ago, so a user with no usable password here is a
		// deprovisioning race or a cleared hash, not a guess to be defended against.
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "current password is wrong"})
		return
	}
	if err := auth.Verify(body.CurrentPassword, cred.PasswordHash); err != nil {
		if !errors.Is(err, auth.ErrMismatch) {
			s.log.Error("stored password hash is unusable", "username", sess.Username, "err", err)
		}
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "current password is wrong"})
		return
	}

	hash, err := auth.Hash(body.NewPassword)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not change password"})
		return
	}
	// The caller's own session is spared; every other one dies with the old password. A cookie is
	// absent when the caller arrived on an API key, in which case there is no session to spare.
	keep := ""
	if c, cerr := r.Cookie("lightship_session"); cerr == nil {
		keep = c.Value
	}
	revoked, err := s.st.SetOwnPassword(r.Context(), sess.UserID, hash, keep)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "auth.password_change", "", map[string]any{"sessions_revoked": revoked})
	writeJSON(w, http.StatusOK, map[string]any{
		"username": sess.Username, "sessions_revoked": revoked})
}

// minPasswordLen is the shortest self-chosen password accepted. Generated ones are far longer.
const minPasswordLen = 8

// A real argon2id hash of a value nobody knows, used to keep the failure path's timing similar to
// the success path.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
