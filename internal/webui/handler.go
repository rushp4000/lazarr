package webui

import (
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
)

//go:embed assets/templates/index.html
var assetFS embed.FS

// handler is the concrete Web UI http.Handler. It holds the provider and the
// compiled template set.
type handler struct {
	prov  Provider
	tmpl  *template.Template
}

// New returns an http.Handler serving the Lazarr dashboard. Wrap it in an
// http.Server and mount on config.WebUI.Listen. When username + password are
// non-empty, all routes are protected by HTTP Basic Auth.
//
// Requests to non-API paths fall through to the index.html shell page.
func New(prov Provider, username, password string) (http.Handler, error) {
	sub, err := fs.Sub(assetFS, "assets/templates")
	if err != nil {
		return nil, err
	}
	tmpl, err := template.ParseFS(sub, "index.html")
	if err != nil {
		return nil, err
	}

	h := &handler{prov: prov, tmpl: tmpl}

	mux := http.NewServeMux()

	// JSON API — read-only.
	mux.HandleFunc("GET /api/status", h.handleStatus)
	mux.HandleFunc("GET /api/releases", h.handleReleases)
	mux.HandleFunc("GET /api/materialized", h.handleMaterialized)
	mux.HandleFunc("GET /api/metrics-summary", h.handleMetricsSummary)
	mux.HandleFunc("GET /api/config", h.handleConfig)

	// JSON API — mutating (POST).
	mux.HandleFunc("POST /api/releases/{hash}/release", h.handleForceRelease)
	mux.HandleFunc("POST /api/audit/run", h.handleAuditRun)
	mux.HandleFunc("POST /api/repair/scan", h.handleRepairScan)
	mux.HandleFunc("POST /api/repair/{hash}/forget", h.handleForgetRelease)

	// Repair — read-only.
	mux.HandleFunc("GET /api/repair", h.handleRepair)

	// Serve the HTML shell for all other paths (SPA-style).
	mux.HandleFunc("/", h.handleIndex)

	return basicAuth(mux, username, password), nil
}

// handleIndex serves the single-page HTML shell.
func (s *handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", nil); err != nil {
		slog.Warn("webui: render index", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
