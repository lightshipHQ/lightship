package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lightshipHQ/lightship/internal/store"
)

func TestDemoStatusPublishesConfiguredCredential(t *testing.T) {
	s := &Server{demo: DemoConfig{
		Username: "admin", Password: "shown",
		DatasetFrom: "2026-09-08T15:19:32Z", DatasetTo: "2026-09-08T16:19:32Z",
	}}
	w := httptest.NewRecorder()
	s.demoStatus(w, httptest.NewRequest(http.MethodGet, "/demo", nil))
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || got["enabled"] != true || got["username"] != "admin" ||
		got["password"] != "shown" || got["dataset_from"] != "2026-09-08T15:19:32Z" ||
		got["dataset_to"] != "2026-09-08T16:19:32Z" {
		t.Fatalf("status %d body %#v", w.Code, got)
	}
}

func TestDemoStatusIsDisabledByDefault(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.demoStatus(w, httptest.NewRequest(http.MethodGet, "/demo", nil))
	if got := w.Body.String(); w.Code != http.StatusOK || got != "{\"enabled\":false}\n" {
		t.Fatalf("status %d body %q", w.Code, got)
	}
}

func TestDemoRouteIsRegisteredOnlyInDemoMode(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/demo", nil)

	regular := httptest.NewRecorder()
	New(nil, nil, nil, nil).ServeHTTP(regular, request)
	if regular.Code != http.StatusNotFound {
		t.Fatalf("regular server status = %d, want %d", regular.Code, http.StatusNotFound)
	}

	demo := httptest.NewRecorder()
	New(nil, nil, nil, nil, WithDemo(DemoConfig{
		Username: "admin",
		Password: "shown",
	})).ServeHTTP(demo, request)
	if demo.Code != http.StatusOK {
		t.Fatalf("demo server status = %d, want %d", demo.Code, http.StatusOK)
	}
}

func TestDemoPasswordIsLockedForPublishedAccount(t *testing.T) {
	s := &Server{demo: DemoConfig{Username: "admin", Password: "shown"}}
	called := false
	h := s.demoPasswordLocked(func(http.ResponseWriter, *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodPost, "/me/password", nil)
	req = req.WithContext(context.WithValue(req.Context(), sessionKey,
		&store.Session{Username: "admin"}))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("got status %d called=%v", w.Code, called)
	}
}

func TestPasswordRouteRemainsAvailableOutsideDemoAccount(t *testing.T) {
	s := &Server{demo: DemoConfig{Username: "admin", Password: "shown"}}
	called := false
	h := s.demoPasswordLocked(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/me/password", nil)
	req = req.WithContext(context.WithValue(req.Context(), sessionKey,
		&store.Session{Username: "operator"}))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusNoContent || !called {
		t.Fatalf("got status %d called=%v", w.Code, called)
	}
}

func TestDemoKeyExpiryDefaultsToAndIsCappedAtOneDay(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	policy := keyExpiryPolicy{Default: 24 * time.Hour, Maximum: 24 * time.Hour}
	expires, err := keyExpiry("", policy, now)
	if err != nil || expires == nil || !expires.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("default expiry = %v, %v", expires, err)
	}
	if _, err := keyExpiry("25h", policy, now); err == nil {
		t.Fatal("demo key longer than 24 hours was accepted")
	}
	expires, err = keyExpiry("1h", policy, now)
	if err != nil || expires == nil || !expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("short expiry = %v, %v", expires, err)
	}
}

func TestRegularKeyMayRemainUnexpired(t *testing.T) {
	expires, err := keyExpiry("", keyExpiryPolicy{}, time.Now())
	if err != nil || expires != nil {
		t.Fatalf("expiry = %v, %v", expires, err)
	}
}
