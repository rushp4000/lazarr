package webui_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/materialize"
	"github.com/rushp4000/lazarr/internal/metrics"
	"github.com/rushp4000/lazarr/internal/webui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProvider is a test implementation of webui.Provider.
type fakeProvider struct {
	status        webui.StatusSnapshot
	releases      []*catalog.Release
	relTotal      int
	matSet        []webui.MaterializedItem
	summary       *metrics.Summary
	summaryErr    error
	releaseErr    error
	auditErr      error
	releasedHashes []string
	auditCalled    bool
	cfg           webui.SafeConfig
}

func (f *fakeProvider) Status() webui.StatusSnapshot { return f.status }
func (f *fakeProvider) ListReleases(_ catalog.ReleaseFilter) ([]*catalog.Release, int, error) {
	return f.releases, f.relTotal, nil
}
func (f *fakeProvider) MaterializedSet() []webui.MaterializedItem { return f.matSet }
func (f *fakeProvider) MetricsSummary() (*metrics.Summary, error) {
	return f.summary, f.summaryErr
}
func (f *fakeProvider) ForceRelease(hash string) error {
	if f.releaseErr != nil {
		return f.releaseErr
	}
	f.releasedHashes = append(f.releasedHashes, hash)
	return nil
}
func (f *fakeProvider) TriggerAudit() error {
	f.auditCalled = true
	return f.auditErr
}
func (f *fakeProvider) TriggerRepairScan(_ context.Context) ([]materialize.RepairEntry, error) {
	return nil, nil
}
func (f *fakeProvider) ListEvicted() ([]*catalog.Release, error) { return nil, nil }
func (f *fakeProvider) ForgetRelease(_ string) error             { return nil }
func (f *fakeProvider) SafeConfig() webui.SafeConfig             { return f.cfg }

func newHandler(t *testing.T, prov webui.Provider) http.Handler {
	t.Helper()
	h, err := webui.New(prov, "", "")
	require.NoError(t, err)
	return h
}

func newHandlerAuth(t *testing.T, prov webui.Provider, user, pass string) http.Handler {
	t.Helper()
	h, err := webui.New(prov, user, pass)
	require.NoError(t, err)
	return h
}

// ─── index ────────────────────────────────────────────────────────────────────

func TestIndex_ServesHTML(t *testing.T) {
	h := newHandler(t, &fakeProvider{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, rec.Body.String(), "Lazarr")
}

// ─── /api/status ─────────────────────────────────────────────────────────────

func TestAPIStatus_Shape(t *testing.T) {
	prov := &fakeProvider{
		status: webui.StatusSnapshot{
			Version:       "v1.0.0",
			UptimeSeconds: 120,
			Mounted:       true,
			SlotsInUse:    1,
			SlotsTotal:    3,
			LastAuditUnix: time.Now().Unix(),
		},
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got webui.StatusSnapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "v1.0.0", got.Version)
	assert.True(t, got.Mounted)
	assert.Equal(t, 1, got.SlotsInUse)
	assert.Equal(t, 3, got.SlotsTotal)
}

// ─── /api/releases ────────────────────────────────────────────────────────────

func TestAPIReleases_Shape(t *testing.T) {
	prov := &fakeProvider{
		releases: []*catalog.Release{
			{Hash: "aabbccddeeff00112233445566778899aabbccdd", Name: "Big Buck Bunny", Category: "radarr_hin", State: catalog.StateVirtual},
		},
		relTotal: 1,
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/releases", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got struct {
		Releases []*catalog.Release `json:"releases"`
		Total    int                `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Releases, 1)
	assert.Equal(t, "Big Buck Bunny", got.Releases[0].Name)
}

func TestAPIReleases_EmptyIsArray(t *testing.T) {
	h := newHandler(t, &fakeProvider{releases: nil, relTotal: 0})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/releases", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"releases":[]`)
}

// ─── /api/materialized ────────────────────────────────────────────────────────

func TestAPIMaterialized_Shape(t *testing.T) {
	prov := &fakeProvider{
		matSet: []webui.MaterializedItem{
			{Hash: "aabbcc001122334455667788990011223344556677", TorBoxID: 42, Refs: 1},
		},
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/materialized", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got []webui.MaterializedItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, int64(42), got[0].TorBoxID)
}

func TestAPIMaterialized_EmptyIsArray(t *testing.T) {
	h := newHandler(t, &fakeProvider{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/materialized", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "[]")
}

// ─── /api/metrics-summary ─────────────────────────────────────────────────────

func TestAPIMetricsSummary_Shape(t *testing.T) {
	prov := &fakeProvider{
		summary: &metrics.Summary{GrabsTotal: 5, MaterializesTotal: 3, ReleasesTotal: 2},
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/metrics-summary", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got metrics.Summary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, float64(5), got.GrabsTotal)
}

func TestAPIMetricsSummary_Error(t *testing.T) {
	prov := &fakeProvider{summaryErr: errors.New("gather failed")}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/metrics-summary", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ─── /api/config ─────────────────────────────────────────────────────────────

func TestAPIConfig_RedactsSecrets(t *testing.T) {
	prov := &fakeProvider{
		cfg: webui.SafeConfig{
			TorBoxAPIBase: "https://api.torbox.app/v1/api",
			Categories:    []string{"radarr_hin"},
		},
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/config", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	// api_key must not appear in the body
	body := rec.Body.String()
	assert.NotContains(t, body, "api_key")
	assert.Contains(t, body, "torbox_api_base")
}

// ─── POST /api/releases/{hash}/release ────────────────────────────────────────

func TestForceRelease_CallsProvider(t *testing.T) {
	prov := &fakeProvider{}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/releases/aabbcc001122334455667788990011223344556677/release", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, prov.releasedHashes, 1)
	assert.Equal(t, "aabbcc001122334455667788990011223344556677", prov.releasedHashes[0])
}

func TestForceRelease_ProviderError(t *testing.T) {
	prov := &fakeProvider{releaseErr: errors.New("engine busy")}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/releases/abc/release", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ─── POST /api/audit/run ──────────────────────────────────────────────────────

func TestAuditRun_CallsProvider(t *testing.T) {
	prov := &fakeProvider{}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit/run", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, prov.auditCalled)
}

func TestAuditRun_ProviderError(t *testing.T) {
	prov := &fakeProvider{auditErr: errors.New("torbox down")}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit/run", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ─── auth middleware ──────────────────────────────────────────────────────────

func TestAuth_NoCredsReturns401(t *testing.T) {
	h := newHandlerAuth(t, &fakeProvider{}, "admin", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_WrongPasswordReturns401(t *testing.T) {
	h := newHandlerAuth(t, &fakeProvider{}, "admin", "secret")
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.SetBasicAuth("admin", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_CorrectCredsPass(t *testing.T) {
	h := newHandlerAuth(t, &fakeProvider{}, "admin", "secret")
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuth_DisabledWhenNoCreds(t *testing.T) {
	h := newHandlerAuth(t, &fakeProvider{}, "", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// ─── disabled when listen empty ────────────────────────────────────────────────

func TestWebUI_DisabledWhenListenEmpty(t *testing.T) {
	// If listen is empty, main.go skips creating the server entirely; this test
	// confirms New() still works and handler still responds (the listen-empty guard
	// is in main.go, not in the handler).
	h := newHandler(t, &fakeProvider{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}
