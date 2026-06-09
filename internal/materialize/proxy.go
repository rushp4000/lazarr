package materialize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/constants"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// cdnHostSuffix is the production host-pin: presigned CDN URLs must terminate at a
// *.tb-cdn.io host (observed: nexus-138.snam.tb-cdn.io). Verified live (docs/08 + docs/11).
const cdnHostSuffix = ".tb-cdn.io"

// proxyTimeouts: a single header-region / range read is small (<= window + readahead), so
// generous-but-bounded timeouts suffice and protect against slowloris-style stalls.
const (
	proxyDialTimeout    = 10 * time.Second
	proxyTLSTimeout     = 10 * time.Second
	proxyRespHeaderWait = 30 * time.Second
	proxyTotalTimeout   = 5 * time.Minute // whole ranged GET (covers slow large-window reads)
)

// errSSRFBlocked is returned when a URL fails the host-pin / scheme / private-IP checks
// BEFORE any network request is made.
var errSSRFBlocked = errors.New("materialize: CDN URL blocked by host-pin/SSRF policy")

// proxy issues SSRF-safe ranged GETs to the presigned CDN URL. Security (docs/15 §4.F):
//   - require https + pin the host to *.tb-cdn.io (plus a configurable allowlist),
//   - refuse private/loopback/link-local IPs at BOTH the URL-validation stage AND at dial
//     time (the dial-time check closes the DNS-rebinding TOCTOU window),
//   - never follow a redirect to a disallowed host,
//   - never log a URL bearing token=/credentials (redactURL).
type proxy struct {
	hc *http.Client

	// allowExtraHosts is the test-only seam. In production it is EMPTY, so only *.tb-cdn.io
	// passes. Tests stand up an httptest.Server on 127.0.0.1 and call allowHost() to permit
	// that exact host:port WITHOUT weakening the production default. allowLoopback gates the
	// dial-time loopback rejection so the httptest server is reachable in happy-path tests.
	allowExtraHosts map[string]struct{}
	allowLoopback   bool
}

// newProxy builds the SSRF-safe proxy with the production default (no extra hosts, loopback
// refused). The dialer re-validates the resolved IP of every connection.
func newProxy() *proxy {
	p := &proxy{allowExtraHosts: make(map[string]struct{})}

	dialer := &net.Dialer{Timeout: proxyDialTimeout}
	transport := &http.Transport{
		Proxy: nil, // never honor environment proxies for outbound CDN fetches
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Re-resolve and validate the IP we are about to connect to. This blocks DNS
			// rebinding: even if the hostname passed validateURL, the resolved address must
			// not be a private/loopback/link-local range (unless explicitly allowed in tests).
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("materialize: resolve %q: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("materialize: no addresses for %q", host)
			}
			for _, ip := range ips {
				if !p.ipAllowed(ip.IP) {
					return nil, fmt.Errorf("%w: resolved private/loopback IP %s", errSSRFBlocked, ip.IP)
				}
			}
			// Pin the dial to a validated IP (avoids a second, racy resolution by the dialer).
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
		TLSHandshakeTimeout:   proxyTLSTimeout,
		ResponseHeaderTimeout: proxyRespHeaderWait,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
	}

	p.hc = &http.Client{
		Transport: transport,
		Timeout:   proxyTotalTimeout,
		// Refuse any redirect whose target fails the same host-pin/SSRF policy.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("materialize: too many redirects")
			}
			if err := p.validateURL(req.URL); err != nil {
				return err
			}
			return nil
		},
	}
	return p
}

// allowHost adds an exact host (or host:port) to the test-only allowlist and enables
// loopback dialing. PRODUCTION CODE NEVER CALLS THIS — it is the host-pin test seam
// described in testdata/cdn/README.md and docs/15 §4.F.
func (p *proxy) allowHost(host string) {
	p.allowExtraHosts[strings.ToLower(host)] = struct{}{}
	p.allowLoopback = true
}

// close releases idle keep-alive connections.
func (p *proxy) close() {
	if t, ok := p.hc.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// validateURL enforces scheme + host-pin BEFORE any request. It rejects non-https, hosts
// outside *.tb-cdn.io (unless allow-listed in tests), and literal private/loopback IP hosts.
// Hostname resolution to private IPs is additionally caught at dial time.
func (p *proxy) validateURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: nil url", errSSRFBlocked)
	}
	host := strings.ToLower(u.Hostname())

	// Test-only allowlist: an exact host or host:port match bypasses the suffix pin (but
	// still flows through the dial-time IP check, which allowLoopback relaxes).
	if _, ok := p.allowExtraHosts[host]; ok {
		return nil
	}
	if _, ok := p.allowExtraHosts[strings.ToLower(u.Host)]; ok {
		return nil
	}

	if u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q (require https)", errSSRFBlocked, u.Scheme)
	}
	// If the host is a literal IP, reject private/loopback/link-local outright.
	if ip := net.ParseIP(host); ip != nil {
		if !p.ipAllowed(ip) {
			return fmt.Errorf("%w: literal private/loopback IP %s", errSSRFBlocked, ip)
		}
		// A public literal IP still isn't a pinned tb-cdn.io host -> reject.
		return fmt.Errorf("%w: literal IP host %s not pinned to %s", errSSRFBlocked, ip, cdnHostSuffix)
	}
	// Host-suffix pin. Guard against a "evil.com.tb-cdn.io.attacker" trick by requiring the
	// suffix to be a real label boundary (host ends with ".tb-cdn.io").
	if host != strings.TrimPrefix(cdnHostSuffix, ".") && !strings.HasSuffix(host, cdnHostSuffix) {
		return fmt.Errorf("%w: host %q not under %s", errSSRFBlocked, host, cdnHostSuffix)
	}
	return nil
}

// ipAllowed reports whether an IP may be dialed. Private, loopback, link-local,
// unspecified, and multicast ranges are refused (SSRF). Loopback is permitted only when
// the test seam enabled it.
func (p *proxy) ipAllowed(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return p.allowLoopback
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return true
}

// proxyRead resolves a fresh dl_link for (ent.hash, fileID) and range-proxies the requested
// window into p. On a 4xx expiry (constants.LinkRefreshStatuses) it invalidates the cached
// link, calls RequestDL exactly once more, and retries the GET exactly once (the #179 fix —
// no infinite loop). Returns (bytesRead, headerBodyForProbeCache, error). headerBody is
// non-nil only when the read covers the header region and probe caching may want it.
func (m *materializer) proxyRead(ctx context.Context, ent *entry, fileID int, p []byte, off int64) (int, []byte, error) {
	link, err := m.freshLink(ctx, ent, fileID, false)
	if err != nil {
		return 0, nil, err
	}

	n, body, err := m.prox.getRange(ctx, link.URL, p, off, m.readahead, m.probe != nil)
	if err == nil {
		return n, body, nil
	}

	// Refresh-on-4xx: invalidate + re-request ONCE, retry ONCE.
	if errors.Is(err, torbox.ErrLinkExpired) {
		m.log.Debug("dl_link expired, refreshing once", "hash", short(ent.hash), "file_id", fileID)
		link, rerr := m.freshLink(ctx, ent, fileID, true)
		if rerr != nil {
			return 0, nil, rerr
		}
		n, body, err = m.prox.getRange(ctx, link.URL, p, off, m.readahead, m.probe != nil)
		if err != nil {
			// A second expiry (or any error) does NOT loop — surface it.
			return 0, nil, fmt.Errorf("materialize: range read after refresh %s: %w", short(ent.hash), err)
		}
		return n, body, nil
	}
	return 0, nil, fmt.Errorf("materialize: range read %s: %w", short(ent.hash), err)
}

// freshLink returns a usable dl_link for (hash,fileID). If forceRefresh, or the cached link
// is missing/near-expiry, it calls RequestDL and persists the new link. nearExpiry uses a
// small skew so we refresh before the CDN starts returning 4xx.
func (m *materializer) freshLink(ctx context.Context, ent *entry, fileID int, forceRefresh bool) (*catalog.DLLink, error) {
	if !forceRefresh {
		if l, err := m.store.GetLink(ent.hash, fileID); err == nil && l != nil && !m.nearExpiry(l) {
			return l, nil
		}
	}
	url, err := m.tb.RequestDL(ent.torboxID, fileID)
	if err != nil {
		// RequestDL errors are already redacted by the torbox client (no token leak).
		return nil, fmt.Errorf("materialize: requestdl %s file %d: %w", short(ent.hash), fileID, err)
	}
	now := m.now().Unix()
	l := &catalog.DLLink{
		Hash:      ent.hash,
		FileID:    fileID,
		URL:       url,
		FetchedAt: now,
		ExpiresAt: parseExpires(url, now),
	}
	if err := m.store.SetLink(l); err != nil {
		// Non-fatal for the read itself (we have a usable URL), but log it.
		m.log.Warn("materialize: persist dl_link failed", "hash", short(ent.hash), "err", err)
	}
	return l, nil
}

// nearExpiry reports whether a cached link is within the refresh skew of expiry.
func (m *materializer) nearExpiry(l *catalog.DLLink) bool {
	if l.ExpiresAt <= 0 {
		return false // unknown expiry -> trust until the CDN 4xxes (refresh-on-4xx handles it)
	}
	const skew = int64(60) // seconds
	return m.now().Unix()+skew >= l.ExpiresAt
}

// getRange performs the SSRF-safe ranged GET. It validates the URL (host-pin/scheme/IP)
// BEFORE issuing the request, sends Range: bytes=off-(off+len-1), and copies exactly the
// requested window into p. wantBody==true returns the read bytes for the probe cache.
//
// readahead bounds how far past the window the CDN MAY send (we ask for window+readahead
// but only ever fill p). On a 4xx in LinkRefreshStatuses it returns torbox.ErrLinkExpired.
func (p *proxy) getRange(ctx context.Context, rawURL string, dst []byte, off, readahead int64, wantBody bool) (int, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: unparseable url", errSSRFBlocked)
	}
	// SECURITY GATE: validate before any network egress. A negative test asserts this
	// rejects non-tb-cdn / private-IP URLs before a GET is ever attempted.
	if err := p.validateURL(u); err != nil {
		return 0, nil, err
	}

	want := int64(len(dst))
	// Ask for the window plus the bounded readahead; the server may send less (it caps at
	// EOF). We never read more than `want` bytes into dst, so readahead only smooths the
	// underlying TCP/HTTP fetch, never the bytes we surface or store.
	last := off + want + readahead - 1
	if last < off {
		last = off + want - 1 // overflow guard
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, redactURL(err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, last))

	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, nil, redactURL(err)
	}
	defer drainClose(resp.Body)

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// 206: body begins at `off` as requested — the normal CDN path.
	case http.StatusOK:
		// 200: the server ignored Range and is returning the WHOLE entity from
		// byte 0. Reading the first len(dst) bytes is only correct at off==0; for
		// off>0 those bytes are the file START, not the requested window. Fail
		// rather than silently corrupt a seek. (TorBox CDN returns 206; this is a
		// guard against an anomalous server/cache.)
		if off > 0 {
			return 0, nil, fmt.Errorf("materialize: CDN ignored Range (HTTP 200) at offset %d", off)
		}
	default:
		if isRefreshStatus(resp.StatusCode) {
			return 0, nil, torbox.ErrLinkExpired
		}
		return 0, nil, fmt.Errorf("materialize: CDN HTTP %d", resp.StatusCode)
	}

	// Fill exactly the requested window. io.ReadFull tolerates a short final read (EOF) via
	// ErrUnexpectedEOF, which is fine for the tail of a file.
	n, rerr := io.ReadFull(resp.Body, dst)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		return n, nil, redactURL(rerr)
	}

	var body []byte
	if wantBody && n > 0 {
		body = make([]byte, n)
		copy(body, dst[:n])
	}
	return n, body, nil
}

// isRefreshStatus reports whether status is one of constants.LinkRefreshStatuses {400,403,410}.
func isRefreshStatus(status int) bool {
	for _, s := range constants.LinkRefreshStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// drainClose drains and closes a response body so the connection can be reused, with a cap
// so a hostile/huge body can't be drained forever.
func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	_ = rc.Close()
}

// parseExpires extracts the unix `expires=` query param from a CDN URL, if present. Returns
// 0 when absent/unparseable (treated as unknown by nearExpiry).
func parseExpires(rawURL string, fallback int64) int64 {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	v := u.Query().Get("expires")
	if v == "" {
		return 0
	}
	var secs int64
	if _, err := fmt.Sscan(v, &secs); err != nil || secs <= 0 {
		return 0
	}
	return secs
}

// redactURL strips the query string from a *url.Error so a presigned token/expires is never
// logged (mirrors torbox.redactURLError; docs/15 §4.A). Non-url.Error values pass through.
func redactURL(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	redacted := "[redacted]"
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.RawQuery = ""
		redacted = u.String()
	}
	return fmt.Errorf("%s %q: %w", ue.Op, redacted, ue.Err)
}

// containsFold reports a case-insensitive substring match.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
