package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/arkan/llm-pico/internal/httpx"
)

// webAssets holds the dashboard. Embedding it makes the binary
// self-contained: there is no static directory to ship or to get wrong.
//
//go:embed all:web
var webAssets embed.FS

// dashboardFS is the embedded tree rooted at the dashboard's index.html.
func dashboardFS() fs.FS {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		// Unreachable: the embed directive guarantees the tree exists.
		panic(err)
	}
	return sub
}

// contentTypes maps file extensions to response types.
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".html":  "text/html; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
}

// static serves the dashboard.
//
// Unknown paths fall back to index.html so the single-page router can handle
// client-side routes on reload. Path traversal is impossible here: fs.ValidPath
// and the embedded tree both reject names that escape the root, which is why
// the Python implementation's manual prefix check on resolved paths is not
// needed.
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

	if p == "" {
		s.serveIndex(w, r)
		return
	}
	// /dashboard and its sub-paths serve the SPA.
	if p == "dashboard" {
		http.Redirect(w, r, "/admin/dashboard/", http.StatusTemporaryRedirect)
		return
	}

	assets := dashboardFS()
	name := p

	if data, err := fs.ReadFile(assets, name); err == nil {
		ct := contentTypes[strings.ToLower(path.Ext(name))]
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	s.serveIndex(w, r)
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboardFS(), "index.html")
	if err != nil {
		httpx.Internal("dashboard assets are missing from this build").Write(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The dashboard is a live view of changing data; caching it would hide
	// deployments behind a stale shell.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
