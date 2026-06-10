package materialize

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// TestAuditTOS_FreshAddGrace guards the drift-grace fix: a release materialized
// seconds ago that mylist does not show yet (TorBox propagation lag, observed live
// 2026-06-10) must NOT be WARNed as drift; one materialized long ago must still be.
func TestAuditTOS_FreshAddGrace(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	now := time.Now().Unix()
	store.addRelease(&catalog.Release{
		Hash: "fresh", State: catalog.StateMaterialized, TorBoxID: 2000,
		MaterializedAt: now - 30, // 30s ago — inside the grace window
	})
	store.addRelease(&catalog.Release{
		Hash: "stale", State: catalog.StateMaterialized, TorBoxID: 3000,
		MaterializedAt: now - 3600, // 1h ago — real drift
	})
	tb.myList = []torbox.TorrentDetail{} // account shows neither

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	var buf bytes.Buffer
	m.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := m.AuditTOS(); err != nil {
		t.Fatalf("AuditTOS: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("torbox_id=2000")) {
		t.Fatalf("fresh add must be grace-skipped, got drift warn:\n%s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("torbox_id=3000")) {
		t.Fatalf("old missing id must still warn as drift:\n%s", buf.String())
	}
}
