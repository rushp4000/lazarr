// Package metrics is Lazarr's observability surface: a set of Prometheus
// collectors wired at the materialize/qbit call sites, plus the HTTP handlers
// for the opt-in admin server (/metrics and /health).
//
// The collectors live on a private registry so importing this package has no
// effect on any global state and tests get a clean slate via a fresh registry.
// Helper funcs (IncGrabs, SetSlotsInUse, …) are cheap and goroutine-safe; the
// engine and qbit layers call them directly without holding a metrics handle.
package metrics

import (
	"encoding/json"
	"net/http"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// reg is the dedicated registry. We do not use the default registry so Lazarr's
// metrics are self-contained and a test can build an isolated handler.
var reg = prometheus.NewRegistry()

var (
	grabs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_grabs_total",
		Help: "Total releases grabbed (symlinked at add, no TorBox add).",
	})
	materializes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_materializes_total",
		Help: "Total lazy materializations (TorBox adds triggered by a read).",
	})
	releases = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_releases_total",
		Help: "Total releases (TorBox deletes by idle/max-hold reaper, LRU, or shutdown).",
	})
	linkRefresh = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_link_refresh_total",
		Help: "Total presigned-link refreshes after a stale-link 4xx.",
	})
	createRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_createtorrent_ratelimited_total",
		Help: "Total createtorrent attempts rejected by the TorBox rate limit.",
	})
	probeHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_probe_cache_hits_total",
		Help: "Header-region reads served from the probe cache (no CDN fetch).",
	})
	probeMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_probe_cache_misses_total",
		Help: "Header-region reads that fell through the probe cache to the live proxy.",
	})
	materializedCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_materialized_count",
		Help: "Releases currently materialized (held on the TorBox account).",
	})
	slotsInUse = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_slots_in_use",
		Help: "Active materialize slots in use.",
	})
	tosAuditLeaks = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_tos_audit_leaks",
		Help: "Leaked torrents found by the last ToS audit (account holds something believed released).",
	})
	reaperSkipped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_reaper_skipped_total",
		Help: "Reaper sweeps skipped because the FUSE mount was unhealthy (reaping paused; alert if rising).",
	})
)

func init() {
	reg.MustRegister(
		grabs, materializes, releases, linkRefresh, createRateLimited,
		probeHits, probeMisses, materializedCount, slotsInUse, tosAuditLeaks,
		reaperSkipped,
	)
}

// Counter/gauge mutators — called at the wiring sites. All are goroutine-safe
// (prometheus collectors are). Kept as plain funcs so call sites stay terse.

func IncGrabs()             { grabs.Inc() }
func IncMaterializes()      { materializes.Inc() }
func IncReleases()          { releases.Inc() }
func IncLinkRefresh()       { linkRefresh.Inc() }
func IncCreateRateLimited() { createRateLimited.Inc() }
func IncProbeHit()          { probeHits.Inc() }
func IncProbeMiss()         { probeMisses.Inc() }
func IncReaperSkipped()     { reaperSkipped.Inc() }

func SetMaterializedCount(n int) { materializedCount.Set(float64(n)) }
func SetSlotsInUse(n int)        { slotsInUse.Set(float64(n)) }
func SetTosAuditLeaks(n int)     { tosAuditLeaks.Set(float64(n)) }

// Summary is a snapshot of the current counter/gauge values for the Web UI.
// All counter fields are cumulative totals since process start.
type Summary struct {
	GrabsTotal             float64 `json:"grabs_total"`
	MaterializesTotal      float64 `json:"materializes_total"`
	ReleasesTotal          float64 `json:"releases_total"`
	LinkRefreshTotal       float64 `json:"link_refresh_total"`
	CreateRateLimitedTotal float64 `json:"create_ratelimited_total"`
	ProbeHitsTotal         float64 `json:"probe_hits_total"`
	ProbeMissesTotal       float64 `json:"probe_misses_total"`
	MaterializedCount      float64 `json:"materialized_count"`
	SlotsInUse             float64 `json:"slots_in_use"`
	TosAuditLeaks          float64 `json:"tos_audit_leaks"`
	ReaperSkippedTotal     float64 `json:"reaper_skipped_total"`
}

// GatherSummary reads the current counter/gauge values from the Prometheus registry.
// It is cheap (gather + scan) and safe for concurrent use.
func GatherSummary() (*Summary, error) {
	families, err := reg.Gather()
	if err != nil {
		return nil, err
	}
	s := &Summary{}
	for _, f := range families {
		val := firstMetricValue(f)
		switch f.GetName() {
		case "lazarr_grabs_total":
			s.GrabsTotal = val
		case "lazarr_materializes_total":
			s.MaterializesTotal = val
		case "lazarr_releases_total":
			s.ReleasesTotal = val
		case "lazarr_link_refresh_total":
			s.LinkRefreshTotal = val
		case "lazarr_createtorrent_ratelimited_total":
			s.CreateRateLimitedTotal = val
		case "lazarr_probe_cache_hits_total":
			s.ProbeHitsTotal = val
		case "lazarr_probe_cache_misses_total":
			s.ProbeMissesTotal = val
		case "lazarr_materialized_count":
			s.MaterializedCount = val
		case "lazarr_slots_in_use":
			s.SlotsInUse = val
		case "lazarr_tos_audit_leaks":
			s.TosAuditLeaks = val
		case "lazarr_reaper_skipped_total":
			s.ReaperSkippedTotal = val
		}
	}
	return s, nil
}

// firstMetricValue extracts the scalar value from the first metric in a family,
// handling Counter, Gauge, and Untyped kinds. Returns 0 for unknown kinds.
func firstMetricValue(f *dto.MetricFamily) float64 {
	if f == nil {
		return 0
	}
	ms := f.GetMetric()
	if len(ms) == 0 {
		return 0
	}
	m := ms[0]
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}
	if u := m.GetUntyped(); u != nil {
		return u.GetValue()
	}
	return 0
}

// MetricsHandler serves the Prometheus exposition of Lazarr's registry.
func MetricsHandler() http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// HealthProvider supplies the live values reported by /health. main wires the
// concrete materialize engine + vfs mount in; tests pass a fake.
type HealthProvider interface {
	Mounted() bool
	SlotsInUse() int
	SlotsTotal() int
	LastAuditUnix() int64
	Version() string
}

// health is the JSON body returned by /health.
type health struct {
	Mounted       bool   `json:"mounted"`
	SlotsInUse    int    `json:"slots_in_use"`
	SlotsTotal    int    `json:"slots_total"`
	LastAuditUnix int64  `json:"last_audit_unix"`
	Version       string `json:"version"`
}

// HealthHandler returns a handler that reports liveness + a snapshot of engine
// state as JSON. It always returns 200 (it is a liveness probe, not a readiness
// gate); the `mounted` field carries the FUSE health signal for scrapers.
func HealthHandler(p HealthProvider) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := health{
			Mounted:       p.Mounted(),
			SlotsInUse:    p.SlotsInUse(),
			SlotsTotal:    p.SlotsTotal(),
			LastAuditUnix: p.LastAuditUnix(),
			Version:       p.Version(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}
