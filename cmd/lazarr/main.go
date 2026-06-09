// Command lazarr is a self-hosted, ToS-compliant TorBox lazy-materialize shim that
// presents as a qBittorrent client to Sonarr/Radarr. See /root/Github/Lazarr/docs.
//
// This is the scaffold/foundation: it defines the package contracts and wiring order.
// Phase 1 (Agents T/Q/C/S) implements torbox/qbit/catalog/symlink; Phase 2 (V/M) adds
// vfs/materialize. See docs/09-build-subagent-plan.md.
package main

import (
	"flag"
	"log"

	"github.com/rushp4000/lazarr/internal/config"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("lazarr: load config %q: %v", *cfgPath, err)
	}

	log.Printf("lazarr scaffold: qbit=%s fuse=%s download_dir=%s slots=%d uncached=%v categories=%v",
		cfg.QBit.Listen, cfg.Paths.FuseMount, cfg.Paths.DownloadDir,
		cfg.Policy.ActiveSlots, cfg.Policy.AllowUncached, cfg.Categories)

	// Wiring order (foundation contract):
	//   store   := catalog.OpenSQLite(...)            // Agent C
	//   tb      := torbox.New(cfg.TorBox, ...)         // Agent T
	//   sym     := symlink.New(cfg.Paths, ...)         // Agent S
	//   qsrv    := qbit.New(qbit.Deps{cfg, store, tb, sym})  // Agent Q  -> http.ListenAndServe(cfg.QBit.Listen, qsrv)
	//   eng     := materialize.New(materialize.Deps{store, tb, cfg.Policy.ActiveSlots}) // Agent M
	//   fs      := vfs.New(cfg.Paths.FuseMount, store, eng)  // Agent V (mounts FUSE)
	//   go eng.AuditTOS loop; go reapers
	log.Printf("lazarr: scaffold only — packages awaiting implementation. See docs/09. Exiting.")
}
