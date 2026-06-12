// Package version carries the build version string. It is a tiny standalone
// package (stdlib-only imports) so it can be referenced from main, the admin /health
// endpoint, and the release tooling without import cycles.
//
// The value is overridable at build time via ldflags, e.g. GoReleaser sets:
//
//	-X github.com/rushp4000/lazarr/internal/version.Version=v1.0.0
package version

import "runtime"

// Version is the build version. "dev" for un-stamped local builds.
var Version = "dev"

// UserAgent identifies Lazarr to TorBox's API and CDN (stdlib-only). TorBox's abuse
// system flags "misconfigured automated tools"; an identifying UA is basic API
// citizenship and lets TorBox attribute traffic correctly (their Cloudflare layer is
// known to block some anonymous/default UAs outright).
func UserAgent() string {
	return "Lazarr/" + Version + " (" + runtime.GOOS + "; " + runtime.GOARCH + ")"
}
