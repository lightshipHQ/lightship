package httpapi

import (
	"net/http"
	"time"
)

// DemoConfig deliberately publishes one ordinary account credential for a disposable demo.
type DemoConfig struct {
	Username    string
	Password    string
	DatasetFrom string
	DatasetTo   string
}

func (c DemoConfig) enabled() bool { return c.Username != "" && c.Password != "" }

func WithDemo(config DemoConfig) Option {
	return func(s *Server) {
		s.demo = config
		s.keyExpiry = keyExpiryPolicy{Default: 24 * time.Hour, Maximum: 24 * time.Hour}
	}
}

func (s *Server) demoStatus(w http.ResponseWriter, _ *http.Request) {
	if !s.demo.enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "username": s.demo.Username, "password": s.demo.Password,
		"dataset_from": s.demo.DatasetFrom, "dataset_to": s.demo.DatasetTo,
	})
}

// The shared demo administrator may edit the access model and create short-lived API keys, but it
// cannot invalidate the credential published on the sign-in screen.
func (s *Server) demoPasswordLocked(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sess := sessionFrom(r.Context()); s.demo.enabled() && sess != nil &&
			sess.Username == s.demo.Username {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "password is fixed in demo mode"})
			return
		}
		next(w, r)
	}
}
