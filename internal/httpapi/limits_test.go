package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lightshipHQ/lightship/internal/traces"
)

func TestSaturatedGateReturnsRetryable429(t *testing.T) {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	w := httptest.NewRecorder()
	if acquireGate(w, gate) {
		t.Fatal("saturated gate was acquired")
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("got %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestQueryLimitAddsDeadlineAndReleasesGate(t *testing.T) {
	s := &Server{limits: ResourceLimits{QueryTimeout: time.Second}, queryGate: make(chan struct{}, 1)}
	called := 0
	h := s.queryLimited(func(w http.ResponseWriter, r *http.Request) {
		called++
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("query context has no deadline")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodGet, "/traces/id", nil).WithContext(context.Background()))
		if w.Code != http.StatusNoContent {
			t.Fatalf("request %d: got %d", i, w.Code)
		}
	}
	if called != 2 {
		t.Fatalf("handler called %d times", called)
	}
}

func TestMaximumQueryWindow(t *testing.T) {
	s := &Server{limits: ResourceLimits{MaxQueryWindow: 24 * time.Hour}}
	now := time.Now()
	if err := s.checkWindow(traces.Window{From: now.Add(-25 * time.Hour), To: now}); err == nil {
		t.Fatal("oversized query window was accepted")
	}
	if err := s.checkWindow(traces.Window{From: now.Add(-time.Hour), To: now}); err != nil {
		t.Fatalf("bounded query window was rejected: %v", err)
	}
}
