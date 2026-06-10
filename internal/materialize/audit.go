package materialize

import (
	"fmt"

	"github.com/rushp4000/lazarr/internal/constants"
	"github.com/rushp4000/lazarr/internal/metrics"
)

// AuditTOS is the compliance proof (docs/12 §guardrail 2). It diffs TorBox's mylist against
// what Lazarr believes it has materialized and alarms on leaks — anything the account still
// holds within LAZARR'S SCOPE that Lazarr believes is released.
//
// The account is SHARED with decypharr (which hoards ~440 items) during canary/coexistence,
// so the audit is scoped to Lazarr-added torrent_ids only: it never inspects, and never
// alarms on, ids Lazarr never added. Scoping uses the union of (a) ids Lazarr currently
// believes are materialized (Store.MaterializedIDs) and (b) ids Lazarr has added during
// this process lifetime (the seen set), minus what is currently believed-held.
//
// Returns an error only on an API failure; a detected leak is logged/alarmed, not returned
// as an error (a leak is an operational alarm, not a call failure).
// auditDriftGrace suppresses the drift WARN for items materialized very recently:
// TorBox's mylist lags a fresh createtorrent by up to ~a minute even with
// bypass_cache=true (observed live 2026-06-10: add at 05:06:10, mylist still
// missing it at the 05:07:12 audit, present minutes later). A fresh add that has
// not propagated yet is not drift.
const auditDriftGrace = 10 * 60 // seconds

func (m *materializer) AuditTOS() error {
	matRels, err := m.store.MaterializedReleases()
	if err != nil {
		return fmt.Errorf("materialize: audit: materialized releases: %w", err)
	}
	believedSet := make(map[int64]struct{}, len(matRels))
	matAt := make(map[int64]int64, len(matRels)) // id -> MaterializedAt (drift grace)
	for _, r := range matRels {
		believedSet[r.TorBoxID] = struct{}{}
		matAt[r.TorBoxID] = r.MaterializedAt
	}
	// on_cache_miss=wait downloads legitimately sit on the account while TorBox
	// fetches them — believed-held, never a leak. The qbit wait-poller owns their
	// removal (complete or bail).
	dlRels, err := m.store.DownloadingReleases()
	if err != nil {
		return fmt.Errorf("materialize: audit: downloading releases: %w", err)
	}
	for _, r := range dlRels {
		believedSet[r.TorBoxID] = struct{}{}
		matAt[r.TorBoxID] = r.MaterializedAt
	}

	// Lazarr's scope = ids we currently believe held + ids we ever added this lifetime.
	scope := make(map[int64]struct{}, len(believedSet))
	for id := range believedSet {
		scope[id] = struct{}{}
	}
	m.mu.Lock()
	for id := range m.seen {
		scope[id] = struct{}{}
	}
	for _, ent := range m.track {
		// In-memory truth: these are genuinely materialized right now.
		believedSet[ent.torboxID] = struct{}{}
		scope[ent.torboxID] = struct{}{}
	}
	m.mu.Unlock()

	// Pull the whole account (paged), but only reason about ids in Lazarr's scope.
	held := make(map[int64]struct{})
	for offset := 0; ; offset += constants.MyListPageMax {
		page, err := m.tb.MyList(offset)
		if err != nil {
			return fmt.Errorf("materialize: audit: mylist offset %d: %w", offset, err)
		}
		if len(page) == 0 {
			break
		}
		for _, d := range page {
			held[d.ID] = struct{}{}
		}
		if len(page) < constants.MyListPageMax {
			break
		}
	}

	var leaks, missing int
	// Leak: an id within Lazarr's scope that the account still holds but that we do NOT
	// currently believe is materialized => we released it (or never tracked it) yet it
	// lingers. This is the ToS alarm.
	for id := range scope {
		_, stillHeld := held[id]
		_, believedHeld := believedSet[id]
		if stillHeld && !believedHeld {
			leaks++
			m.log.Error("TOS AUDIT: leaked torrent on account (believed released)", "torbox_id", id)
		}
	}
	// Drift (informational): we believe an id is held but the account does not have it
	// (TorBox purged it, or an out-of-band delete). Not a ToS violation; surfaced for ops.
	// Items materialized within the grace window are skipped — mylist propagation lag,
	// not drift.
	now := m.now().Unix()
	for id := range believedSet {
		if _, ok := held[id]; !ok {
			if at, known := matAt[id]; known && at > 0 && now-at < auditDriftGrace {
				m.log.Debug("TOS AUDIT: fresh add not yet visible in mylist (grace)", "torbox_id", id)
				continue
			}
			missing++
			m.log.Warn("TOS AUDIT: materialized id not present on account (drift)", "torbox_id", id)
		}
	}

	if leaks == 0 && missing == 0 {
		m.log.Info("TOS AUDIT: clean", "in_scope", len(scope), "believed_held", len(believedSet))
	}
	metrics.SetTosAuditLeaks(leaks)
	m.lastAudit.Store(m.now().Unix())
	return nil
}
