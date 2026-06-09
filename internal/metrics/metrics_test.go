package metrics_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rushp4000/lazarr/internal/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMetricsHandler_ExposesAndIncrements verifies the /metrics handler emits valid
// Prometheus exposition text and that a counter helper actually moves the needle.
func TestMetricsHandler_ExposesAndIncrements(t *testing.T) {
	h := metrics.MetricsHandler()

	scrape := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}

	body := scrape()
	// HELP/TYPE lines + the metric names must be present.
	for _, name := range []string{
		"lazarr_grabs_total",
		"lazarr_materializes_total",
		"lazarr_releases_total",
		"lazarr_link_refresh_total",
		"lazarr_createtorrent_ratelimited_total",
		"lazarr_probe_cache_hits_total",
		"lazarr_probe_cache_misses_total",
		"lazarr_materialized_count",
		"lazarr_slots_in_use",
		"lazarr_tos_audit_leaks",
	} {
		assert.Contains(t, body, name, "exposition must list %s", name)
	}
	assert.Contains(t, body, "# TYPE lazarr_grabs_total counter")

	// A helper call must be reflected in the next scrape.
	require.Contains(t, scrape(), "lazarr_grabs_total 0")
	metrics.IncGrabs()
	assert.Contains(t, scrape(), "lazarr_grabs_total 1")

	metrics.SetSlotsInUse(2)
	assert.Contains(t, scrape(), "lazarr_slots_in_use 2")
}

// fakeProvider is a static metrics.HealthProvider for the /health test.
type fakeProvider struct {
	mounted    bool
	inUse, tot int
	audit      int64
	ver        string
}

func (f fakeProvider) Mounted() bool        { return f.mounted }
func (f fakeProvider) SlotsInUse() int      { return f.inUse }
func (f fakeProvider) SlotsTotal() int      { return f.tot }
func (f fakeProvider) LastAuditUnix() int64 { return f.audit }
func (f fakeProvider) Version() string      { return f.ver }

// TestHealthHandler_ReturnsJSON verifies /health returns the documented JSON shape.
func TestHealthHandler_ReturnsJSON(t *testing.T) {
	h := metrics.HealthHandler(fakeProvider{mounted: true, inUse: 1, tot: 3, audit: 1780000000, ver: "v1.2.3"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"))

	var got struct {
		Mounted       bool   `json:"mounted"`
		SlotsInUse    int    `json:"slots_in_use"`
		SlotsTotal    int    `json:"slots_total"`
		LastAuditUnix int64  `json:"last_audit_unix"`
		Version       string `json:"version"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.Mounted)
	assert.Equal(t, 1, got.SlotsInUse)
	assert.Equal(t, 3, got.SlotsTotal)
	assert.Equal(t, int64(1780000000), got.LastAuditUnix)
	assert.Equal(t, "v1.2.3", got.Version)
}
