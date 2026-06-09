package webui

import (
	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/metrics"
)

// Provider is the webui data contract. main wires concrete implementations via the
// adapter in cmd/lazarr/main.go. Tests pass a fake.
//
// Provider must never expose torbox.api_key. SafeConfig() must redact it.
type Provider interface {
	// Status returns a live snapshot for /api/status.
	Status() StatusSnapshot
	// ListReleases delegates to catalog.Store.ListReleases for the releases table.
	ListReleases(f catalog.ReleaseFilter) ([]*catalog.Release, int, error)
	// MaterializedSet returns a snapshot of the live in-memory materialized set.
	MaterializedSet() []MaterializedItem
	// MetricsSummary returns the current counter/gauge values for sparklines/charts.
	MetricsSummary() (*metrics.Summary, error)
	// ForceRelease calls engine.Release for a single hash. Used by the force-release
	// action in the UI; mutating, POST-only.
	ForceRelease(hash string) error
	// TriggerAudit runs the ToS audit immediately (engine.AuditTOS). Mutating, POST-only.
	TriggerAudit() error
	// SafeConfig returns the effective config with api_key and passwords redacted.
	SafeConfig() SafeConfig
}
