package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lightshipHQ/lightship/internal/schema"
)

type testSource struct {
	mu        sync.Mutex
	model     schema.Model
	err       error
	reads     int
	afterRead func()
}

func (s *testSource) LoadModel(context.Context) (schema.Model, error) {
	s.mu.Lock()
	s.reads++
	m, err, after := s.model, s.err, s.afterRead
	s.mu.Unlock()
	if after != nil {
		after()
	}
	return m, err
}

func (s *testSource) set(m schema.Model, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model, s.err = m, err
}

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func testCache(s *testSource, clock *testClock) *Cache {
	c := New(s, time.Minute)
	c.now = clock.now
	return c
}

func grantModel() schema.Model {
	return schema.Model{Version: 1, Roles: []schema.Role{{Name: "reader", Policies: []schema.Policy{{Title: "grant", Expression: "true"}}}}}
}

func requireVersion(t *testing.T, c *Cache, version int64) {
	t.Helper()
	m, err := c.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema.Version != version {
		t.Fatalf("version = %d, want %d", m.Schema.Version, version)
	}
}

func requireDenied(t *testing.T, c *Cache) {
	t.Helper()
	m, err := c.Get(context.Background())
	if err == nil || m != nil {
		t.Fatalf("expired grant returned: model=%v err=%v", m, err)
	}
}

func TestReplicasDenyExpiredGrantsAndRecover(t *testing.T) {
	clock := &testClock{at: time.Now()}
	source := &testSource{model: grantModel()}
	writer, replica := testCache(source, clock), testCache(source, clock)
	requireVersion(t, writer, 1)
	requireVersion(t, replica, 1)

	// A successful policy revocation is observed immediately by the writer.
	revoked := schema.Model{Version: 2}
	source.set(revoked, nil)
	if _, err := writer.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireVersion(t, writer, 2)
	requireVersion(t, replica, 1)

	// Other replicas may retain the grant only until the original deadline, not indefinitely.
	source.set(revoked, errors.New("database unavailable"))
	clock.advance(time.Minute - time.Nanosecond)
	requireVersion(t, replica, 1)
	clock.advance(time.Nanosecond)
	requireDenied(t, replica)
	requireDenied(t, writer)
	clock.advance(time.Hour)
	requireDenied(t, replica)
	source.set(revoked, nil)
	requireVersion(t, replica, 2)
	m, err := replica.Get(context.Background())
	if err != nil || len(m.Registry["reader"]) != 0 {
		t.Fatalf("revoked grant retained: %v %v", m, err)
	}
}

func TestFailedExplicitRefreshInvalidatesUnexpiredGrant(t *testing.T) {
	clock := &testClock{at: time.Now()}
	source := &testSource{model: grantModel()}
	c := testCache(source, clock)
	requireVersion(t, c, 1)
	source.set(schema.Model{Version: 2}, errors.New("refresh failed after commit"))
	if m, err := c.Refresh(context.Background()); err == nil || m != nil {
		t.Fatal("refresh must fail")
	}
	requireDenied(t, c)
	source.set(schema.Model{Version: 2}, nil)
	requireVersion(t, c, 2)
}

func TestInvalidReloadDoesNotExtendGrant(t *testing.T) {
	clock := &testClock{at: time.Now()}
	source := &testSource{model: grantModel()}
	c := testCache(source, clock)
	requireVersion(t, c, 1)
	bad := grantModel()
	bad.Version = 2
	bad.Roles[0].Policies[0].Expression = "unknown_field == true"
	source.set(bad, nil)
	clock.advance(time.Minute)
	requireDenied(t, c)
	requireDenied(t, c)
}

func TestSlowLoadCannotRenewOldSnapshot(t *testing.T) {
	clock := &testClock{at: time.Now()}
	source := &testSource{model: grantModel(), afterRead: func() { clock.advance(time.Minute) }}
	c := testCache(source, clock)
	requireDenied(t, c)
	if c.cur != nil {
		t.Fatal("already expired load was cached")
	}
}

func TestSnapshotAgeIncludesReadTime(t *testing.T) {
	clock := &testClock{at: time.Now()}
	source := &testSource{model: grantModel(), afterRead: func() { clock.advance(40 * time.Second) }}
	c := testCache(source, clock)
	requireVersion(t, c, 1)
	source.set(schema.Model{}, errors.New("unavailable"))
	clock.advance(20 * time.Second)
	requireDenied(t, c)
}

func TestColdCacheAndConcurrentRefresh(t *testing.T) {
	clock := &testClock{at: time.Now()}
	source := &testSource{err: errors.New("unavailable")}
	c := testCache(source, clock)
	requireDenied(t, c)
	source.set(grantModel(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := c.Get(context.Background())
			if err != nil || m == nil || m.Schema.Version != 1 {
				t.Errorf("Get = %v, %v", m, err)
			}
		}()
	}
	wg.Wait()
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.reads != 2 {
		t.Fatalf("reads = %d, want cold failure + one successful read", source.reads)
	}
}
