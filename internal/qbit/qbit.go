// Package qbit emulates the qBittorrent WebUI API for the *arr suite. Endpoints, the
// torrents/info field set, and the "report complete from checkcached size" trick are in
// docs/03-arr-qbit-integration.md. Built by Agent Q (docs/09). Use the qbit-emu skill.
package qbit

import (
	"net/http"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/symlink"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// Deps are what the qBit server needs (wired in main).
type Deps struct {
	Config  *config.Config
	Store   catalog.Store
	TorBox  torbox.Client
	Symlink symlink.Manager
	// Engine releases an in-flight materialization on torrents/delete so a delete during
	// playback (upgrade-replace, Phase-4 import churn) frees the TorBox item + slot instead
	// of leaking it (S2). Optional and nil-safe: qbit tests that wire no engine pass nil.
	Engine Releaser
}

// Releaser is the slice of the materialize engine qbit needs: release a materialized
// release by infohash. Satisfied by *materialize.materializer via its Release method.
type Releaser interface {
	Release(hash string) error
}

// Server is the qBittorrent-emulation HTTP handler. New(Deps) (built by Agent Q)
// returns something satisfying http.Handler, mounting /api/v2/* per docs/03.
type Server interface {
	http.Handler
}
