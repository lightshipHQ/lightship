package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lightshipHQ/lightship/internal/store"
)

func TestModelConflictIsRetryableHTTPConflict(t *testing.T) {
	w := httptest.NewRecorder()
	storeErr(w, fmt.Errorf("persist model: %w", store.ErrModelConflict))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reload and retry") {
		t.Fatalf("conflict lacks retry guidance: %s", w.Body.String())
	}
}
