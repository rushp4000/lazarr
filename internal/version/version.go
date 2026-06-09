// Package version carries the build version string. It is a tiny standalone
// package (no imports) so it can be referenced from main, the admin /health
// endpoint, and the release tooling without import cycles.
//
// The value is overridable at build time via ldflags, e.g. GoReleaser sets:
//
//	-X github.com/rushp4000/lazarr/internal/version.Version=v1.0.0
package version

// Version is the build version. "dev" for un-stamped local builds.
var Version = "dev"
