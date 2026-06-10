package materialize

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/rushp4000/lazarr/internal/config"
)

// TestReadAt_DeadCDNNodeRefreshesLink guards the fix for the live 2026-06-10 outage:
// a presigned URL pinned to a CDN node that has died (connection refused / header
// timeout) never returns a 4xx, so the old refresh-on-4xx path kept retrying the same
// dead URL forever. A transport-level failure must invalidate the cached link, call
// RequestDL once (TorBox re-pins a healthy node), and retry once.
func TestReadAt_DeadCDNNodeRefreshesLink(t *testing.T) {
	content := make([]byte, 64<<10)
	for i := range content {
		content[i] = byte(i % 251)
	}

	// Healthy node: the standard fake CDN serving real ranged bytes.
	cdn := newFakeCDN(content)
	defer cdn.close()

	// Dead node: a server we close immediately -> connection refused at that addr.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	deadURL := dead.URL
	dead.Close()

	store := newFakeStore()
	tb := newFakeTorBox()
	var calls atomic.Int32
	tb.dlURLFn = func(_ int64, fileID int) string {
		// First RequestDL hands out the dead node, the refresh hands out the live one.
		if calls.Add(1) == 1 {
			return fmt.Sprintf("%s/dl/h/%d/f.mp4?token=x&expires=9999999999", deadURL, fileID)
		}
		return cdn.url("h", fileID)
	}

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 2}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	for _, raw := range []string{cdn.srv.URL, deadURL} {
		u, _ := url.Parse(raw)
		m.AllowTestHost(u.Host)
		m.AllowTestHost(u.Hostname())
	}
	defer func() { _ = m.Close() }()
	newRelease(store, "h", "magnet:?xt=urn:btih:h")

	p := make([]byte, 4096)
	n, err := m.ReadAt("h", 0, p, 8192)
	if err != nil {
		t.Fatalf("read through dead node should refresh + succeed, got: %v", err)
	}
	if !bytes.Equal(p[:n], content[8192:8192+int64(n)]) {
		t.Fatalf("bytes mismatch after dead-node refresh")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("RequestDL calls = %d, want 2 (initial + one refresh)", got)
	}
}
