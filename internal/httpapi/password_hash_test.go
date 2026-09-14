package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckHashUsesCanonicalProfile(t *testing.T) {
	valid := "$argon2id$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for _, encoded := range []string{"", valid} {
		if !checkHash(httptest.NewRecorder(), encoded) {
			t.Fatal("empty or canonical hash rejected")
		}
	}
	for _, encoded := range []string{"plaintext", "$argon2id$", strings.Replace(valid, "t=3", "t=0", 1), valid[:len(valid)-43]} {
		rec := httptest.NewRecorder()
		if checkHash(rec, encoded) || rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid hash accepted: status %d", rec.Code)
		}
	}
}

func TestUserHandlersRejectInvalidHashBeforeStoreAccess(t *testing.T) {
	s := &Server{}
	for name, handler := range map[string]http.HandlerFunc{"create": s.createUser, "update": s.updateUser} {
		t.Run(name, func(t *testing.T) {
			body := `{"password_hash":"$argon2id$v=19$m=65536,t=0,p=4$invalid$"}`
			if name == "create" {
				body = `{"username":"alice","password_hash":"$argon2id$v=19$m=65536,t=0,p=4$invalid$"}`
			}
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}
