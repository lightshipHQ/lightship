package httpapi

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
)

// The UI is a static page that talks to the same JSON API an API-only deployment exposes. It has
// no query path of its own, so there is no second place a policy filter could be forgotten — which
// is the same reason the ClickHouse driver has one importer.
//
//go:embed ui/*
var uiFS embed.FS

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, "ui unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page renders trace attributes, which are untrusted input. It uses textContent
	// throughout; this is the backstop if that ever slips.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self'; "+
			"font-src 'self'; connect-src 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}

// uiAsset serves the JavaScript modules, stylesheets, and fonts embedded with the page. Only a
// single path segment with an allowed extension is accepted; the route is not a general view into
// the embedded filesystem.
func (s *Server) uiAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ext := path.Ext(name)
	if (ext != ".js" && ext != ".css" && ext != ".woff2") || path.Base(name) != name {
		http.NotFound(w, r)
		return
	}
	body, err := fs.ReadFile(uiFS, "ui/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	contentType := "text/javascript; charset=utf-8"
	if ext == ".css" {
		contentType = "text/css; charset=utf-8"
	} else if ext == ".woff2" {
		contentType = "font/woff2"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}
