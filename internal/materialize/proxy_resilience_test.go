package materialize

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/config"
)

// TestReadAt_429RetryAfterHonored guards the v1.1.3 patient-throttle fix (observed live
// 2026-06-12: Cloudflare/ERTH 429'd a fresh import's read burst; the old 300/900ms-only
// ladder gave up in ~1.2s and surfaced EIO, killing playback startup). When the CDN sends
// Retry-After, the wait must honor it (when larger than the ladder step) and the read must
// still succeed — the player sees buffering, never an error.
func TestReadAt_429RetryAfterHonored(t *testing.T) {
	content := make([]byte, 32<<10)
	for i := range content {
		content[i] = byte(i % 199)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rng := r.Header.Get("Range")
		var a, b int64
		_, _ = fmtSscanfRange(rng, &a, &b)
		if b >= int64(len(content)) {
			b = int64(len(content)) - 1
		}
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(a, 10)+"-"+strconv.FormatInt(b, 10)+"/"+strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[a : b+1])
	}))
	defer srv.Close()

	store := newFakeStore()
	tb := newFakeTorBox()
	tb.dlURLFn = func(_ int64, fileID int) string {
		return srv.URL + "/dl/f.mp4?token=x&expires=9999999999"
	}
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 2}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())
	u, _ := url.Parse(srv.URL)
	m.AllowTestHost(u.Host)
	m.AllowTestHost(u.Hostname())
	defer func() { _ = m.Close() }()
	newRelease(store, "h", "magnet:?xt=urn:btih:h")

	p := make([]byte, 4096)
	start := time.Now()
	n, err := m.ReadAt("h", 0, p, 0)
	if err != nil {
		t.Fatalf("read through a Retry-After 429 should wait+retry+succeed, got: %v", err)
	}
	if !bytes.Equal(p[:n], content[:n]) {
		t.Fatalf("bytes mismatch after Retry-After retry")
	}
	// Ladder step 0 is 300ms; the server's Retry-After of 1s must win.
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("Retry-After: 1 not honored; read returned after %v (< ~1s)", elapsed)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 CDN GETs (429 then 206), got %d", got)
	}
}

// TestThrottleBreaker_PausesPrefetch guards the prefetch circuit breaker: after any
// observed 429 the prefetcher must schedule nothing speculative until the pause window
// elapses, so the foreground (player-blocking) read gets the CDN's whole budget.
func TestThrottleBreaker_PausesPrefetch(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 2, ReadaheadWindows: 4, ReadaheadChunkMiB: 1}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())
	defer func() { _ = m.Close() }()

	base := time.Now()
	m.SetNow(func() time.Time { return base })

	var fetches atomic.Int32
	pf := newPrefetcher(4, 1, func(_ context.Context, _ *entry, _ int, _ []byte, _ int64) (int, error) {
		fetches.Add(1)
		return 0, nil
	})

	m.noteThrottle()
	if !m.throttledNow() {
		t.Fatal("breaker not open immediately after noteThrottle")
	}
	pf.prefetchAsync(m, "h", 0, 1)
	time.Sleep(50 * time.Millisecond)
	if got := fetches.Load(); got != 0 {
		t.Fatalf("prefetch ran while throttled: %d fetches", got)
	}

	// Window elapses -> breaker closes (prefetch may resume).
	m.SetNow(func() time.Time { return base.Add(throttlePause + time.Second) })
	if m.throttledNow() {
		t.Fatal("breaker still open after the pause window elapsed")
	}
}

// TestExtraCDNHosts_ConfigurableHostPin guards the operator escape hatch for new TorBox
// CDN domains (the ERTH lesson: a hardcoded pin broke every stream until a release).
// Extra suffixes must be honored with the same label-boundary rigor as the built-ins,
// and junk entries (single-label, empty) must be ignored rather than widen the pin.
func TestExtraCDNHosts_ConfigurableHostPin(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	m, err := New(Deps{
		Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 2},
		ExtraCDNHosts: []string{"tb-cdn.example", ".Tb-CDN.Example2", ".io", "", "  "},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())
	defer func() { _ = m.Close() }()

	accept := []string{
		"https://nexus.weur.tb-cdn.example/dld/x?token=a", // extra, no leading dot in config
		"https://nexus.erth.tb-cdn.example2/dld/x",        // extra, mixed case in config
		"https://nexus-1.snam.tb-cdn.io/dld/x",            // built-in must survive extras
		"https://nexus.erth.tb-cdn.earth/dld/x",           // built-in .earth
	}
	for _, raw := range accept {
		if err := m.ValidateURLForTest(raw); err != nil {
			t.Errorf("URL %q should pass the pin, got: %v", raw, err)
		}
	}
	reject := []string{
		"https://evil.tb-cdn.example.attacker.net/dld/x", // label-boundary attack on an extra
		"https://eviltb-cdn.example/dld/x",               // no label boundary
		"https://foo.io/dld/x",                           // single-label junk entry must NOT widen the pin
		"http://nexus.weur.tb-cdn.example/dld/x",         // extras never relax the https requirement
	}
	for _, raw := range reject {
		if err := m.ValidateURLForTest(raw); err == nil {
			t.Errorf("URL %q must be rejected by the pin", raw)
		}
	}
}

// TestRetryAfterDuration covers header parsing: delta-seconds, HTTP-date, clamping, junk.
func TestRetryAfterDuration(t *testing.T) {
	now := time.Date(2026, 6, 12, 5, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"2", 2 * time.Second},
		{"0", 0},
		{"9999", maxRetryAfter}, // clamp
		{"-5", 0},
		{"garbage", 0},
		{now.Add(3 * time.Second).UTC().Format(http.TimeFormat), 3 * time.Second},
		{now.Add(-time.Minute).UTC().Format(http.TimeFormat), 0}, // past date -> 0
	}
	for _, c := range cases {
		if got := retryAfterDuration(c.in, now); got != c.want {
			t.Errorf("retryAfterDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
