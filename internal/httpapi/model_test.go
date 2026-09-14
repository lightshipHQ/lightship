package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lightshipHQ/lightship/internal/model"
	"github.com/lightshipHQ/lightship/internal/schema"
)

type failingModelSource struct{ err error }

func (s *failingModelSource) LoadModel(context.Context) (schema.Model, error) {
	return schema.Model{Version: 1}, s.err
}

func TestFailedPostWriteRefreshDeniesCurrentModel(t *testing.T) {
	source := &failingModelSource{}
	cache := model.New(source, time.Minute)
	if _, err := cache.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.err = errors.New("database unavailable")
	if _, err := cache.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh failure")
	}
	s := &Server{models: cache}
	w := httptest.NewRecorder()
	m, ok := s.current(w, httptest.NewRequest(http.MethodGet, "/filter/schema", nil))
	if ok || m != nil || w.Code != http.StatusServiceUnavailable {
		t.Fatalf("current returned stale model: model=%v ok=%v status=%d", m, ok, w.Code)
	}
}
