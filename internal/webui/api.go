package webui

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/logging"
	"github.com/rushp4000/lazarr/internal/materialize"
)

// releasesResponse is the /api/releases JSON body.
type releasesResponse struct {
	Releases []*catalog.Release `json:"releases"`
	Total    int                `json:"total"`
	Limit    int                `json:"limit"`
	Offset   int                `json:"offset"`
}

// jsonOK writes v as JSON with status 200.
func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("webui: json encode", "err", err)
	}
}

// jsonErr writes a JSON {"error":"..."} body with the given status.
func jsonErr(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleStatus serves GET /api/status.
func (s *handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, s.prov.Status())
}

// handleReleases serves GET /api/releases?q=&state=&category=&limit=&offset=.
func (s *handler) handleReleases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := catalog.ReleaseFilter{
		Q:        q.Get("q"),
		State:    catalog.State(q.Get("state")),
		Category: q.Get("category"),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			f.Offset = n
		}
	}

	rels, total, err := s.prov.ListReleases(f)
	if err != nil {
		jsonErr(w, "store error", http.StatusInternalServerError)
		slog.Warn("webui: list releases", "err", err)
		return
	}
	if rels == nil {
		rels = []*catalog.Release{}
	}
	jsonOK(w, releasesResponse{
		Releases: rels,
		Total:    total,
		Limit:    f.Limit,
		Offset:   f.Offset,
	})
}

// handleMaterialized serves GET /api/materialized.
func (s *handler) handleMaterialized(w http.ResponseWriter, r *http.Request) {
	items := s.prov.MaterializedSet()
	if items == nil {
		items = []MaterializedItem{}
	}
	jsonOK(w, items)
}

// handleMetricsSummary serves GET /api/metrics-summary.
func (s *handler) handleMetricsSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.prov.MetricsSummary()
	if err != nil {
		jsonErr(w, "metrics gather error", http.StatusInternalServerError)
		slog.Warn("webui: gather metrics", "err", err)
		return
	}
	jsonOK(w, sum)
}

// handleConfig serves GET /api/config.
func (s *handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, s.prov.SafeConfig())
}

// handleForceRelease serves POST /api/releases/{hash}/release.
func (s *handler) handleForceRelease(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimPrefix(r.PathValue("hash"), "")
	if hash == "" {
		jsonErr(w, "missing hash", http.StatusBadRequest)
		return
	}
	if err := s.prov.ForceRelease(hash); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		slog.Warn("webui: force release", "hash", hash, "err", err)
		return
	}
	jsonOK(w, map[string]string{"status": "released"})
}

// handleAuditRun serves POST /api/audit/run.
func (s *handler) handleAuditRun(w http.ResponseWriter, r *http.Request) {
	if err := s.prov.TriggerAudit(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		slog.Warn("webui: trigger audit", "err", err)
		return
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// handleRepair serves GET /api/repair — returns the cached evicted list from the catalog.
func (s *handler) handleRepair(w http.ResponseWriter, r *http.Request) {
	evicted, err := s.prov.ListEvicted()
	if err != nil {
		jsonErr(w, "store error", http.StatusInternalServerError)
		slog.Warn("webui: list evicted", "err", err)
		return
	}
	if evicted == nil {
		evicted = []*catalog.Release{}
	}
	jsonOK(w, map[string]any{"evicted": evicted, "count": len(evicted)})
}

// handleRepairScan serves POST /api/repair/scan — triggers a live checkcached sweep.
func (s *handler) handleRepairScan(w http.ResponseWriter, r *http.Request) {
	entries, err := s.prov.TriggerRepairScan(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		slog.Warn("webui: repair scan", "err", err)
		return
	}
	if entries == nil {
		entries = []materialize.RepairEntry{}
	}
	jsonOK(w, map[string]any{"evicted": entries, "count": len(entries)})
}

// handleSettingsGet serves GET /api/settings — the editable config with secrets blanked.
func (s *handler) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	st := s.prov.GetSettings()
	// Defense in depth: never let secrets out on GET regardless of provider behavior.
	st.TorBoxAPIKey = ""
	st.WebUIPassword = ""
	jsonOK(w, st)
}

// handleSettingsSave serves POST /api/settings. Validation failures come back as a
// 400 with the human-readable reason so the form can show it inline.
func (s *handler) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	var st Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&st); err != nil {
		jsonErr(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	restart, err := s.prov.SaveSettings(st)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		slog.Warn("webui: save settings rejected", "err", err)
		return
	}
	slog.Info("webui: settings saved", "restart_required", restart)
	jsonOK(w, SaveResult{Saved: true, RestartRequired: restart})
}

// handleLogs serves GET /api/logs?level=&limit=.
func (s *handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	level := r.URL.Query().Get("level")
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	entries := s.prov.Logs(level, limit)
	if entries == nil {
		entries = []logging.Entry{}
	}
	jsonOK(w, map[string]any{"entries": entries, "count": len(entries)})
}

// handleRestart serves POST /api/restart — graceful shutdown so the container
// supervisor restarts Lazarr on the saved config.
func (s *handler) handleRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.prov.Restart(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("webui: restart requested")
	jsonOK(w, map[string]string{"status": "restarting"})
}

// handleForgetRelease serves POST /api/repair/{hash}/forget — removes from catalog + symlinks.
func (s *handler) handleForgetRelease(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" {
		jsonErr(w, "missing hash", http.StatusBadRequest)
		return
	}
	if err := s.prov.ForgetRelease(hash); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		slog.Warn("webui: forget release", "hash", hash, "err", err)
		return
	}
	jsonOK(w, map[string]string{"status": "forgotten"})
}
