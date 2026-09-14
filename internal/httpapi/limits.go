package httpapi

import (
	"context"
	"net/http"
)

func acquireGate(w http.ResponseWriter, gate chan struct{}) bool {
	if gate == nil {
		return true
	}
	select {
	case gate <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "server is busy; retry later"})
		return false
	}
}

func (s *Server) queryLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !acquireGate(w, s.queryGate) {
			return
		}
		if s.queryGate != nil {
			defer func() { <-s.queryGate }()
		}
		if s.limits.QueryTimeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), s.limits.QueryTimeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		next(w, r)
	}
}
