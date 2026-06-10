package materialize

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rushp4000/lazarr/internal/config"
)

// TestReadAt_CDN429BackoffRetry guards the throttle fix (observed live 2026-06-10:
// readahead bursts made the CDN 429 the FOREGROUND read, which surfaced as EIO to the
// player). A 429 must be retried after a short backoff on the SAME link — no refresh,
// no error to the reader — and succeed when the node stops throttling.
func TestReadAt_CDN429BackoffRetry(t *testing.T) {
	content := make([]byte, 64<<10)
	for i := range content {
		content[i] = byte(i % 251)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First two GETs are throttled; the third succeeds.
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rng := r.Header.Get("Range") // "bytes=a-b"
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
	n, err := m.ReadAt("h", 0, p, 8192)
	if err != nil {
		t.Fatalf("read through 429s should backoff+retry+succeed, got: %v", err)
	}
	if !bytes.Equal(p[:n], content[8192:8192+n]) {
		t.Fatalf("bytes mismatch after throttle retries")
	}
	if got := atomic.LoadInt32(&tb.requestDL); got > 1 {
		t.Fatalf("429 must NOT trigger a link refresh; RequestDL called %d times", got)
	}
}

// fmtSscanfRange parses "bytes=a-b".
func fmtSscanfRange(s string, a, b *int64) (int, error) {
	s = strings.TrimPrefix(s, "bytes=")
	parts := strings.SplitN(s, "-", 2)
	var err error
	*a, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, err
	}
	*b, err = strconv.ParseInt(parts[1], 10, 64)
	return 2, err
}
