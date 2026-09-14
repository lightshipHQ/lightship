// Package model holds the compiled access model and keeps it fresh.
//
// The model lives in Postgres, but the query path needs it compiled: CEL parsed, whitelisted and
// turned into programs. Doing that per request would be wasteful, and reading Postgres per request
// would put three more round trips on every query. So each replica holds a compiled snapshot with a
// short TTL.
//
// That TTL is a revocation window, and it is the one cost of moving the model out of a file. A role
// *assignment* is still read fresh on every request, so removing someone's role takes effect
// immediately. Narrowing a *policy* takes effect here within the TTL. The replica that performs a
// write invalidates its own snapshot at once, so a single-replica deployment — the common
// self-hosted case — is immediate; several replicas converge within the TTL.
package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightshipHQ/lightship/internal/policy"
	"github.com/lightshipHQ/lightship/internal/schema"
)

// ErrNoBinding reports that nobody has told lightship which table holds spans. Queries fail closed
// until they do: without a trace id column there is no quantifier, and without a quantifier there
// is no guarantee to enforce.
var ErrNoBinding = errors.New("no source binding: run discovery and set one before querying")

// Compiled is one coherent snapshot. Everything in it was built from the same model version.
type Compiled struct {
	Schema   schema.Model
	Env      *policy.Env
	Registry policy.Registry
	Admin    *policy.Program
}

// Ready reports whether this snapshot can serve a query at all.
func (c *Compiled) Ready() error {
	if c.Schema.Binding.Table == "" {
		return ErrNoBinding
	}
	return c.Schema.Binding.Validate()
}

// Compile turns a stored model into a usable one. It is the single validation path: the write side
// calls it to reject a change before storing it, and the read side calls it to load one. A policy
// that will not compile therefore fails at the request that proposes it, leaving the running system
// untouched — rather than at the next boot, which used to mean an outage caused by a typo.
func Compile(m schema.Model) (*Compiled, error) {
	env, err := policy.NewEnv(m)
	if err != nil {
		return nil, err
	}
	reg, err := policy.BuildRegistry(env, m.Roles)
	if err != nil {
		return nil, err
	}
	admin, err := env.AdminProgram()
	if err != nil {
		return nil, err
	}
	return &Compiled{Schema: m, Env: env, Registry: reg, Admin: admin}, nil
}

// Cache serves the compiled model, reloading when its snapshot is older than the TTL.
type Cache struct {
	st  modelSource
	ttl time.Duration
	now func() time.Time

	mu       sync.RWMutex
	cur      *Compiled
	loadedAt time.Time
	// reload is held across a refresh so that a burst of requests arriving on a cold or stale
	// cache produces one database read rather than one per request.
	reload sync.Mutex
}

type modelSource interface {
	LoadModel(context.Context) (schema.Model, error)
}

func New(st modelSource, ttl time.Duration) *Cache {
	return &Cache{st: st, ttl: ttl, now: time.Now}
}

// Get returns a snapshot younger than the TTL. Once expired, a failed refresh denies access;
// it never extends the lifetime of the previous policies.
func (c *Cache) Get(ctx context.Context) (*Compiled, error) {
	c.mu.RLock()
	cur, at := c.cur, c.loadedAt
	c.mu.RUnlock()
	if cur != nil && c.now().Sub(at) < c.ttl {
		return cur, nil
	}

	c.reload.Lock()
	defer c.reload.Unlock()
	// Another goroutine may have refreshed while this one waited.
	c.mu.RLock()
	cur, at = c.cur, c.loadedAt
	c.mu.RUnlock()
	if cur != nil && c.now().Sub(at) < c.ttl {
		return cur, nil
	}

	return c.refreshLocked(ctx)
}

func (c *Cache) load(ctx context.Context) (*Compiled, error) {
	m, err := c.st.LoadModel(ctx)
	if err != nil {
		return nil, err
	}
	compiled, err := Compile(m)
	if err != nil {
		// Stored state that will not compile is a bug rather than a user error — the write path
		// compiles before it stores — so say so loudly rather than serving something partial.
		return nil, fmt.Errorf("stored access model does not compile (version %d): %w", m.Version, err)
	}
	return compiled, nil
}

// Refresh reloads now and returns the new snapshot. The replica that performs a write calls it, so
// its own next request sees the change without waiting out the TTL.
func (c *Cache) Refresh(ctx context.Context) (*Compiled, error) {
	c.reload.Lock()
	defer c.reload.Unlock()
	// Explicit refresh follows a committed configuration change. Discard the local grant even
	// when reloading fails, so subsequent requests cannot use this replica's previous model.
	c.mu.Lock()
	c.cur = nil
	c.mu.Unlock()
	return c.refreshLocked(ctx)
}

func (c *Cache) refreshLocked(ctx context.Context) (*Compiled, error) {
	// A slow database read or compilation must not renew an old snapshot's lifetime.
	started := c.now()
	next, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now().Sub(started) >= c.ttl {
		return nil, errors.New("access model refresh exceeded cache TTL")
	}
	c.cur, c.loadedAt = next, started
	return next, nil
}

// Validate compiles a proposed model without storing it, so a write can be rejected before it
// becomes the thing everyone is served.
func Validate(m schema.Model) error {
	_, err := Compile(m)
	return err
}
