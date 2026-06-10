// Package qbit implements the qBittorrent WebUI API emulation layer.
// Endpoints, wire formats, and the "report complete from checkcached size" trick
// are described in docs/03-arr-qbit-integration.md and the qbit-emu skill.
package qbit

import (
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/metrics"
	"github.com/rushp4000/lazarr/internal/torbox"
)

const (
	appVersion    = "v4.6.0"
	webapiVersion = "2.9.3"
)

// server is the concrete qBittorrent emulation handler.
type server struct {
	deps  Deps
	mux   *http.ServeMux
	waits *waitPool // on_cache_miss=wait live progress (hash -> progress/eta)
}

// New returns a Server that mounts /api/v2/* on a fresh ServeMux.
func New(d Deps) Server {
	s := &server{deps: d, waits: newWaitPool()}
	mux := http.NewServeMux()

	// Auth
	mux.HandleFunc("POST /api/v2/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v2/auth/logout", s.handleLogout)

	// App info
	mux.HandleFunc("GET /api/v2/app/version", s.handleAppVersion)
	mux.HandleFunc("GET /api/v2/app/webapiVersion", s.handleWebapiVersion)
	mux.HandleFunc("GET /api/v2/app/buildInfo", s.handleBuildInfo)
	mux.HandleFunc("GET /api/v2/app/preferences", s.handlePreferences)
	mux.HandleFunc("GET /api/v2/app/defaultSavePath", s.handleDefaultSavePath)

	// Torrents
	mux.HandleFunc("POST /api/v2/torrents/add", s.handleTorrentsAdd)
	mux.HandleFunc("GET /api/v2/torrents/info", s.handleTorrentsInfo)
	mux.HandleFunc("GET /api/v2/torrents/properties", s.handleTorrentsProperties)
	mux.HandleFunc("GET /api/v2/torrents/files", s.handleTorrentsFiles)
	mux.HandleFunc("POST /api/v2/torrents/delete", s.handleTorrentsDelete)
	mux.HandleFunc("GET /api/v2/torrents/categories", s.handleTorrentsCategories)
	mux.HandleFunc("POST /api/v2/torrents/createCategory", s.handleCreateCategory)
	mux.HandleFunc("POST /api/v2/torrents/removeCategories", s.handleRemoveCategories)
	mux.HandleFunc("POST /api/v2/torrents/setCategory", s.handleSetCategory)
	mux.HandleFunc("POST /api/v2/torrents/pause", s.handleNoop)
	mux.HandleFunc("POST /api/v2/torrents/resume", s.handleNoop)
	mux.HandleFunc("POST /api/v2/torrents/topPrio", s.handleNoop)

	// Sync / transfer
	mux.HandleFunc("GET /api/v2/sync/maindata", s.handleMaindata)
	mux.HandleFunc("GET /api/v2/transfer/info", s.handleTransferInfo)

	s.mux = mux
	return s
}

// statusRecorder captures the response status for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	s.mux.ServeHTTP(rec, r)
	slog.Debug("qbit: req", "method", r.Method, "path", r.URL.Path,
		"query", r.URL.RawQuery, "status", rec.status)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("qbit: json encode", "err", err)
	}
}

func writeText(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(text))
}

// savePath returns the per-category download directory.
func (s *server) savePath(category string) string {
	base := s.deps.Config.Paths.DownloadDir
	if category == "" {
		return base
	}
	return path.Join(base, category)
}

// contentPath returns the full path to the first file in a release's symlink tree.
// For a single-file release it is <save_path>/<name>/<file>. For multi-file it is
// <save_path>/<name> (the arr will scan the directory).
func (s *server) contentPath(r *catalog.Release, files []catalog.File) string {
	sp := s.savePath(r.Category)
	if len(files) == 1 {
		return path.Join(sp, r.Name, files[0].RelPath)
	}
	return path.Join(sp, r.Name)
}

// ── auth ─────────────────────────────────────────────────────────────────────

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Per spec: "accept any/no creds". The arrs configure their own user/pass in
	// the download client settings; Lazarr accepts all to avoid configuration friction.
	// (Optional enforcement can be added in a later pass if needed.)
	_ = r.ParseForm()
	writeText(w, "Ok.")
}

func (s *server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// ── app info ─────────────────────────────────────────────────────────────────

func (s *server) handleAppVersion(w http.ResponseWriter, _ *http.Request) {
	writeText(w, appVersion)
}

func (s *server) handleWebapiVersion(w http.ResponseWriter, _ *http.Request) {
	writeText(w, webapiVersion)
}

func (s *server) handleBuildInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"bitness":    64,
		"boost":      "1.84.0",
		"libtorrent": "2.0.10",
		"openssl":    "3.2.1",
		"qt":         "6.7.1",
		"zlib":       "1.3.1",
	})
}

func (s *server) handlePreferences(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"save_path":                            s.deps.Config.Paths.DownloadDir,
		"temp_path_enabled":                    false,
		"temp_path":                            "",
		"queueing_enabled":                     false,
		"max_active_downloads":                 -1,
		"max_active_uploads":                   -1,
		"max_active_torrents":                  -1,
		"dont_count_slow_torrents":             false,
		"incomplete_files_ext":                 false,
		"create_subfolder_enabled":             true,
		"autorun_enabled":                      false,
		"autorun_program":                      "",
		"listen_port":                          8080,
		"upnp":                                 false,
		"random_port":                          false,
		"dl_limit":                             0,
		"up_limit":                             0,
		"max_connec":                           -1,
		"max_connec_per_torrent":               -1,
		"max_uploads":                          -1,
		"max_uploads_per_torrent":              -1,
		"bittorrent_protocol":                  0,
		"limit_utp_rate":                       true,
		"limit_tcp_overhead":                   false,
		"alt_dl_limit":                         10,
		"alt_up_limit":                         10,
		"scheduler_enabled":                    false,
		"web_ui_username":                      s.deps.Config.QBit.Username,
		"bypass_local_auth":                    false,
		"bypass_auth_subnet_whitelist_enabled": false,
		"use_https":                            false,
		"web_ui_port":                          8080,
	})
}

func (s *server) handleDefaultSavePath(w http.ResponseWriter, _ *http.Request) {
	writeText(w, s.deps.Config.Paths.DownloadDir)
}

// ── torrents/add ─────────────────────────────────────────────────────────────

func (s *server) handleTorrentsAdd(w http.ResponseWriter, r *http.Request) {
	// Bound the whole add upload (magnets + any .torrent) so a hostile or
	// runaway client cannot exhaust memory/disk during form parsing.
	r.Body = http.MaxBytesReader(w, r.Body, 33<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		// Fallback for application/x-www-form-urlencoded
		_ = r.ParseForm()
	}

	category := r.FormValue("category")
	savePath := r.FormValue("savepath")
	if savePath == "" {
		savePath = s.savePath(category)
	}

	// Parse magnet / URL from the `urls` field.
	rawURLs := r.FormValue("urls")
	var hash, name string

	if rawURLs != "" {
		for _, line := range strings.Split(rawURLs, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "magnet:") {
				h, n := parseMagnet(line)
				hash = h
				name = n
				break
			}
		}
	}

	// Check for .torrent file upload.
	if r.MultipartForm != nil {
		if fhs, ok := r.MultipartForm.File["torrents"]; ok && len(fhs) > 0 {
			fh := fhs[0]
			f, err := fh.Open()
			if err == nil {
				defer f.Close()
				h, n, err2 := parseTorrentFile(f)
				if err2 == nil {
					hash = h
					name = n
				}
			}
		}
	}

	if hash == "" {
		slog.Warn("qbit: torrents/add missing hash", "urls", rawURLs)
		http.Error(w, "missing infohash", http.StatusBadRequest)
		return
	}
	hash = strings.ToLower(hash)

	// Infohash becomes the <hash> segment of the symlink TARGET
	// (<fuse_mount>/<hash>/<rel_path>), so it must be a real infohash and never
	// a path-traversal payload (docs/15 §4.C). parseMagnet/parseTorrentFile
	// already normalize to 40-hex, but validate here as the single chokepoint.
	if !isInfohash(hash) {
		slog.Warn("qbit: torrents/add invalid infohash", "hash", hash)
		http.Error(w, "invalid infohash", http.StatusBadRequest)
		return
	}

	if name == "" {
		name = hash // fallback
	}

	now := time.Now().Unix()

	rel := &catalog.Release{
		Hash:      hash,
		Name:      name,
		Category:  category,
		Magnet:    strings.TrimSpace(rawURLs),
		State:     catalog.StateVirtual,
		AddedOn:   now,
		CreatedAt: now,
	}

	// Step 1: CheckCached (no TorBox add).
	cachedMap, err := s.deps.TorBox.CheckCached([]string{hash})
	var files []catalog.File

	if err != nil {
		slog.Warn("qbit: CheckCached error", "hash", hash, "err", err)
		rel.State = catalog.StateError
	} else if item, ok := cachedMap[hash]; ok && len(item.Files) > 0 {
		// Cache hit — build file rows.
		rel.Name = item.Name
		rel.TotalSize = item.Size
		rel.Cached = true
		files = toCatalogFiles(hash, item.Files)
	} else {
		// Cache miss — try TorrentInfo fallback.
		if s.deps.Config.Policy.AllowUncached {
			info, err2 := s.deps.TorBox.TorrentInfo(hash)
			if err2 == nil && info != nil && len(info.Files) > 0 {
				rel.Name = info.Name
				rel.TotalSize = info.Size
				rel.Cached = false
				files = toCatalogFiles(hash, info.Files)
			} else {
				slog.Warn("qbit: TorrentInfo miss/error", "hash", hash, "err", err2)
				rel.State = catalog.StateError
			}
		} else {
			// Cached-only mode: policy decides the miss behavior.
			switch s.deps.Config.Policy.OnCacheMiss {
			case "reject":
				// Refuse the add: the arr marks this release failed and immediately
				// falls back to the next candidate. Nothing is stored.
				slog.Info("qbit: cache miss → rejecting add (on_cache_miss=reject)", "hash", hash, "name", name)
				writeText(w, "Fails.")
				return
			case "wait":
				if s.startWaitDownload(rel) {
					break // accepted as StateDownloading
				}
				// Could not start (cap reached / createtorrent failed) → error state.
				rel.State = catalog.StateError
			default: // "error"
				slog.Info("qbit: cache miss, cached-only → error state", "hash", hash)
				rel.State = catalog.StateError
			}
		}
	}

	// Step 2: Upsert into catalog.
	if err2 := s.deps.Store.UpsertRelease(rel, files); err2 != nil {
		slog.Error("qbit: UpsertRelease", "hash", hash, "err", err2)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	// Step 3: Build symlink tree (only for successful releases).
	if rel.State == catalog.StateVirtual && len(files) > 0 {
		if err2 := s.deps.Symlink.Create(rel, files); err2 != nil {
			slog.Warn("qbit: Symlink.Create", "hash", hash, "err", err2)
			// Non-fatal — arr will see the complete state anyway.
		}
	}

	metrics.IncGrabs()
	slog.Info("qbit: grab",
		"hash", hash, "category", category, "name", rel.Name,
		"size", rel.TotalSize, "cached", rel.Cached, "state", rel.State, "files", len(files))

	writeText(w, "Ok.")
}

// toCatalogFiles converts TorBox CachedFile slice to catalog.File slice.
func toCatalogFiles(hash string, cf []torbox.CachedFile) []catalog.File {
	out := make([]catalog.File, len(cf))
	for i, f := range cf {
		out[i] = catalog.File{
			Hash:    hash,
			FileID:  f.ID,
			RelPath: f.Name,
			Size:    f.Size,
		}
	}
	return out
}

// ── torrents/info ─────────────────────────────────────────────────────────────

// torrentInfoObj is the JSON shape returned in the /torrents/info array.
type torrentInfoObj struct {
	Hash          string  `json:"hash"`
	Name          string  `json:"name"`
	Size          int64   `json:"size"`
	Progress      float64 `json:"progress"`
	State         string  `json:"state"`
	Category      string  `json:"category"`
	SavePath      string  `json:"save_path"`
	ContentPath   string  `json:"content_path"`
	Completed     int64   `json:"completed"`
	AmountLeft    int64   `json:"amount_left"`
	CompletionOn  int64   `json:"completion_on"`
	AddedOn       int64   `json:"added_on"`
	DlSpeed       int64   `json:"dlspeed"`
	UpSpeed       int64   `json:"upspeed"`
	ETA           int64   `json:"eta"`
	Ratio         float64 `json:"ratio"`
	SeqDl         bool    `json:"seq_dl"`
	FLPiecePrio   bool    `json:"f_l_piece_prio"`
	NumSeeds      int     `json:"num_seeds"`
	NumLeechs     int     `json:"num_leechs"`
	NumComplete   int     `json:"num_complete"`
	NumIncomplete int     `json:"num_incomplete"`
	Tags          string  `json:"tags"`
	Tracker       string  `json:"tracker"`
}

func (s *server) releaseToInfoObj(r *catalog.Release, files []catalog.File) torrentInfoObj {
	sp := s.savePath(r.Category)
	cp := s.contentPath(r, files)

	state := "pausedUP"
	progress := 1.0
	amtLeft := int64(0)
	eta := int64(0)
	completionOn := r.AddedOn // we complete instantly

	switch r.State {
	case catalog.StateError:
		state = "error"
		progress = 0.0
		amtLeft = r.TotalSize
		completionOn = 0
	case catalog.StateDownloading:
		// on_cache_miss=wait: TorBox is fetching the torrent; show its real progress
		// so the arr renders a live download bar instead of a fake instant-complete.
		state = "downloading"
		completionOn = 0
		var wp waitProgress
		if s.waits != nil {
			wp, _ = s.waits.get(r.Hash)
		}
		progress = wp.Progress
		eta = wp.ETA
		if r.TotalSize > 0 {
			amtLeft = r.TotalSize - int64(progress*float64(r.TotalSize))
		}
	}

	return torrentInfoObj{
		Hash:         r.Hash,
		Name:         r.Name,
		Size:         r.TotalSize,
		Progress:     progress,
		State:        state,
		Category:     r.Category,
		SavePath:     sp,
		ContentPath:  cp,
		Completed:    r.TotalSize,
		AmountLeft:   amtLeft,
		CompletionOn: completionOn,
		AddedOn:      r.AddedOn,
		DlSpeed:      0,
		UpSpeed:      0,
		ETA:          eta,
		Ratio:        0,
		SeqDl:        false,
		FLPiecePrio:  false,
	}
}

// releasesForQuery resolves the releases a torrents/info or sync/maindata query
// is asking about. The *arr clients are inconsistent: some poll by `hashes` with
// NO category, some by `category`, some with neither (expecting all). We support
// all three so the client can always find the torrent it grabbed.
func (s *server) releasesForQuery(category, hashesParam string) []*catalog.Release {
	switch {
	case hashesParam != "" && hashesParam != "all":
		// Look up the specific hashes directly (no category needed).
		var out []*catalog.Release
		seen := make(map[string]bool)
		for _, h := range strings.Split(hashesParam, "|") {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" || seen[h] {
				continue
			}
			seen[h] = true
			if rel, _, err := s.deps.Store.GetRelease(h); err == nil && rel != nil {
				out = append(out, rel)
			}
		}
		return out
	case category != "":
		rels, err := s.deps.Store.ListByCategory(category)
		if err != nil {
			slog.Error("qbit: ListByCategory", "category", category, "err", err)
		}
		return rels
	default:
		// Neither filter: return all releases across configured categories.
		var out []*catalog.Release
		for _, c := range s.deps.Config.Categories {
			if rels, err := s.deps.Store.ListByCategory(c); err == nil {
				out = append(out, rels...)
			}
		}
		return out
	}
}

func (s *server) handleTorrentsInfo(w http.ResponseWriter, r *http.Request) {
	releases := s.releasesForQuery(r.URL.Query().Get("category"), r.URL.Query().Get("hashes"))

	result := make([]torrentInfoObj, 0, len(releases))
	for _, rel := range releases {
		_, files, err2 := s.deps.Store.GetRelease(rel.Hash)
		if err2 != nil {
			slog.Warn("qbit: GetRelease in info", "hash", rel.Hash, "err", err2)
			files = nil
		}
		result = append(result, s.releaseToInfoObj(rel, files))
	}

	writeJSON(w, result)
}

// ── torrents/properties ───────────────────────────────────────────────────────

func (s *server) handleTorrentsProperties(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(r.URL.Query().Get("hash"))
	if hash == "" {
		http.Error(w, "missing hash", http.StatusBadRequest)
		return
	}

	rel, files, err := s.deps.Store.GetRelease(hash)
	if err != nil || rel == nil {
		http.NotFound(w, r)
		return
	}

	sp := s.savePath(rel.Category)
	writeJSON(w, map[string]any{
		"save_path":                sp,
		"creation_date":            rel.AddedOn,
		"piece_size":               0,
		"comment":                  "",
		"total_wasted":             0,
		"total_uploaded":           0,
		"total_uploaded_session":   0,
		"total_downloaded":         rel.TotalSize,
		"total_downloaded_session": rel.TotalSize,
		"up_limit":                 0,
		"dl_limit":                 0,
		"time_elapsed":             0,
		"seeding_time":             0,
		"nb_connections":           0,
		"nb_connections_limit":     100,
		"share_ratio":              0,
		"addition_date":            rel.AddedOn,
		"completion_date":          rel.AddedOn,
		"created_by":               "Lazarr",
		"dl_speed_avg":             0,
		"dl_speed":                 0,
		"eta":                      0,
		"last_seen":                rel.AddedOn,
		"peers":                    0,
		"peers_total":              0,
		"pieces_have":              0,
		"pieces_num":               0,
		"reannounce":               0,
		"seeds":                    0,
		"seeds_total":              0,
		"total_size":               rel.TotalSize,
		"up_speed_avg":             0,
		"up_speed":                 0,
		"name":                     rel.Name,
		"hash":                     rel.Hash,
		"content_path":             s.contentPath(rel, files),
	})
}

// ── torrents/files ────────────────────────────────────────────────────────────

type torrentFileObj struct {
	Index    int     `json:"index"`
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	Priority int     `json:"priority"`
	IsSeed   bool    `json:"is_seed"`
}

func (s *server) handleTorrentsFiles(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(r.URL.Query().Get("hash"))
	if hash == "" {
		http.Error(w, "missing hash", http.StatusBadRequest)
		return
	}

	_, files, err := s.deps.Store.GetRelease(hash)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	result := make([]torrentFileObj, len(files))
	for i, f := range files {
		result[i] = torrentFileObj{
			Index:    i,
			ID:       f.FileID,
			Name:     f.RelPath,
			Size:     f.Size,
			Progress: 1.0,
			Priority: 1,
			IsSeed:   true,
		}
	}
	writeJSON(w, result)
}

// ── torrents/delete ───────────────────────────────────────────────────────────

func (s *server) handleTorrentsDelete(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	hashesParam := r.FormValue("hashes")
	// deleteFiles := r.FormValue("deleteFiles") == "true"  // reserved for Phase 2

	if hashesParam == "" {
		http.Error(w, "missing hashes", http.StatusBadRequest)
		return
	}

	for _, h := range strings.Split(hashesParam, "|") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if err := s.deps.Symlink.Remove(h); err != nil {
			slog.Warn("qbit: Symlink.Remove", "hash", h, "err", err)
		}
		// S2: release any in-flight (or store-only) materialization BEFORE deleting the row.
		// Order matters — Release consults the store for store-only leftovers (B2), so
		// deleting the row first would orphan the TorBox item. A release error must not fail
		// the delete (the arr expects 200); log and continue.
		if s.deps.Engine != nil {
			if err := s.deps.Engine.Release(h); err != nil {
				slog.Warn("qbit: engine release on delete", "hash", h, "err", err)
			}
		}
		if err := s.deps.Store.DeleteRelease(h); err != nil {
			slog.Warn("qbit: DeleteRelease", "hash", h, "err", err)
		}
	}

	w.WriteHeader(http.StatusOK)
}

// ── categories ────────────────────────────────────────────────────────────────

func (s *server) handleTorrentsCategories(w http.ResponseWriter, _ *http.Request) {
	cats := make(map[string]any, len(s.deps.Config.Categories))
	for _, c := range s.deps.Config.Categories {
		cats[c] = map[string]string{
			"name":      c,
			"save_path": s.savePath(c),
		}
	}
	writeJSON(w, cats)
}

func (s *server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	// Accept and ignore (the arr just tells us its category name).
	_ = r.ParseForm()
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleRemoveCategories(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleSetCategory(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	// We could update catalog entries here but arrs rarely use this after add.
	w.WriteHeader(http.StatusOK)
}

// ── no-op handlers ────────────────────────────────────────────────────────────

func (s *server) handleNoop(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// ── sync/maindata ─────────────────────────────────────────────────────────────

func (s *server) handleMaindata(w http.ResponseWriter, r *http.Request) {
	releases := s.releasesForQuery(r.URL.Query().Get("category"), "")

	torrents := make(map[string]torrentInfoObj, len(releases))
	for _, rel := range releases {
		_, files, _ := s.deps.Store.GetRelease(rel.Hash)
		torrents[rel.Hash] = s.releaseToInfoObj(rel, files)
	}

	cats := make(map[string]any, len(s.deps.Config.Categories))
	for _, c := range s.deps.Config.Categories {
		cats[c] = map[string]string{
			"name":      c,
			"save_path": s.savePath(c),
		}
	}

	writeJSON(w, map[string]any{
		"rid":         1,
		"full_update": true,
		"torrents":    torrents,
		"categories":  cats,
		"server_state": map[string]any{
			"connection_status":  "connected",
			"dl_info_speed":      0,
			"dl_info_data":       0,
			"up_info_speed":      0,
			"up_info_data":       0,
			"dl_rate_limit":      0,
			"up_rate_limit":      0,
			"dht_nodes":          0,
			"free_space_on_disk": 0,
		},
	})
}

// ── transfer/info ─────────────────────────────────────────────────────────────

func (s *server) handleTransferInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"dl_info_speed":        0,
		"dl_info_data":         0,
		"dl_rate_limit":        0,
		"up_info_speed":        0,
		"up_info_data":         0,
		"up_rate_limit":        0,
		"dht_nodes":            0,
		"connection_status":    "connected",
		"use_alt_speed_limits": false,
		"queueing":             false,
	})
}

// ── magnet / torrent parsing ──────────────────────────────────────────────────

// parseMagnet extracts the infohash (normalized to 40-char lowercase hex) and
// display name from a magnet URI. Returns ("", "") if there is no valid btih
// infohash. Both hex (40-char) and base32 (32-char) infohashes are accepted.
func parseMagnet(magnet string) (hash, name string) {
	// magnet:?xt=urn:btih:<hash>&dn=<name>&...
	rest := strings.TrimPrefix(magnet, "magnet:?")
	for _, part := range strings.Split(rest, "&") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		switch k {
		case "xt":
			// urn:btih:<hash> — hex or base32.
			if _, h, found := strings.Cut(v, "btih:"); found {
				hash = normalizeInfohash(h)
			}
		case "dn":
			name = urlDecode(v)
		}
	}
	return
}

// normalizeInfohash returns s as a 40-char lowercase hex infohash, converting a
// 32-char base32 (RFC 4648) infohash to hex. Returns "" if s is neither a valid
// 40-hex nor a valid 32-base32 infohash. This is the traversal guard for the
// <hash> path segment (docs/15 §4.C).
func normalizeInfohash(s string) string {
	switch len(strings.TrimSpace(s)) {
	case 40:
		l := strings.ToLower(strings.TrimSpace(s))
		if isInfohash(l) {
			return l
		}
	case 32:
		// base32 is case-insensitive; std encoding is uppercase, unpadded.
		if b, err := base32.StdEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(s))); err == nil && len(b) == 20 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

// isInfohash reports whether s is exactly 40 lowercase hex chars (a SHA-1 btih).
func isInfohash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// urlDecode percent/plus-decodes a magnet field (e.g. dn), falling back to the
// raw value when it is not valid percent-encoding.
func urlDecode(s string) string {
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return s
}

// maxTorrentBytes caps a .torrent upload so a hostile/oversized file cannot
// exhaust memory. Real torrents are well under this even with many pieces.
const maxTorrentBytes = 10 << 20 // 10 MiB

// parseTorrentFile reads a .torrent (bencode) and extracts infohash + name.
// This is a minimal bencode parser sufficient for info dict extraction.
func parseTorrentFile(r io.Reader) (hash, name string, err error) {
	// Read up to maxTorrentBytes+1 so we can detect an over-limit upload.
	buf, err := io.ReadAll(io.LimitReader(r, maxTorrentBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("torrent: read: %w", err)
	}
	if len(buf) > maxTorrentBytes {
		return "", "", fmt.Errorf("torrent: file exceeds %d bytes", maxTorrentBytes)
	}

	// Find the "info" dictionary in the bencode and sha1 it.
	infoStart, infoEnd, found := findInfoDict(buf)
	if !found {
		return "", "", fmt.Errorf("torrent: info dict not found")
	}

	infoSHA1 := sha1Sum(buf[infoStart:infoEnd])
	hash = fmt.Sprintf("%x", infoSHA1)

	// Extract "name" from info dict.
	name = bencodeFindString(buf[infoStart:infoEnd], "name")
	return hash, name, nil
}
