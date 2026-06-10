// Package logging provides Lazarr's runtime-adjustable logger: a slog handler that
// tees every record into a bounded in-memory ring buffer (for the Web UI Logs tab)
// while writing the normal text stream to stdout. The level is a slog.LevelVar so
// the Web UI settings page can change verbosity live, without a restart.
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// RingCapacity is how many recent records the in-memory buffer keeps. At Lazarr's
// log volume (a few lines per playback event) this covers days of history while
// bounding memory to well under 1 MiB.
const RingCapacity = 1000

// ParseLevel maps a config string to a slog.Level. Empty defaults to info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("unknown log level %q (want debug|info|warn|error)", s)
}

// Entry is one captured log record, pre-flattened for JSON transport to the UI.
type Entry struct {
	TimeUnixMs int64  `json:"time_unix_ms"`
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	Attrs      string `json:"attrs"` // "key=val key=val", groups dotted
}

// Ring is a fixed-capacity, mutex-guarded ring buffer of Entry.
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next int
	full bool
}

// NewRing returns a Ring with the given capacity (<=0 uses RingCapacity).
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = RingCapacity
	}
	return &Ring{buf: make([]Entry, capacity)}
}

// add appends one entry, overwriting the oldest when full.
func (r *Ring) add(e Entry) {
	r.mu.Lock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Snapshot returns up to limit most-recent entries at or above min, oldest first.
// limit <= 0 means all retained entries.
func (r *Ring) Snapshot(min slog.Level, limit int) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]Entry, 0, n)
	start := 0
	if r.full {
		start = r.next // oldest entry when wrapped
	}
	for i := 0; i < n; i++ {
		e := r.buf[(start+i)%len(r.buf)]
		if levelOf(e.Level) >= min {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// levelOf is the inverse of slog.Level.String() for the four standard names.
func levelOf(s string) slog.Level {
	switch s {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Handler tees records into a Ring and forwards them to a base handler. The base
// handler owns formatting and the level gate (give it a *slog.LevelVar); records
// the base would drop are not captured either, so the UI shows exactly what the
// configured level emits.
type Handler struct {
	base  slog.Handler
	ring  *Ring
	attrs string // pre-flattened WithAttrs context
	group string // dotted group prefix from WithGroup
}

// NewHandler wraps base so every record it accepts is also captured into ring.
func NewHandler(base slog.Handler, ring *Ring) *Handler {
	return &Handler{base: base, ring: ring}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	var sb strings.Builder
	if h.attrs != "" {
		sb.WriteString(h.attrs)
	}
	rec.Attrs(func(a slog.Attr) bool {
		appendAttr(&sb, h.group, a)
		return true
	})
	h.ring.add(Entry{
		TimeUnixMs: rec.Time.UnixMilli(),
		Level:      rec.Level.String(),
		Msg:        rec.Message,
		Attrs:      strings.TrimSpace(sb.String()),
	})
	return h.base.Handle(ctx, rec)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var sb strings.Builder
	sb.WriteString(h.attrs)
	for _, a := range attrs {
		appendAttr(&sb, h.group, a)
	}
	return &Handler{base: h.base.WithAttrs(attrs), ring: h.ring, attrs: sb.String(), group: h.group}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &Handler{base: h.base.WithGroup(name), ring: h.ring, attrs: h.attrs, group: g}
}

// appendAttr flattens one attr (recursing into groups) as " key=val".
func appendAttr(sb *strings.Builder, group string, a slog.Attr) {
	if a.Value.Kind() == slog.KindGroup {
		g := a.Key
		if group != "" {
			g = group + "." + a.Key
		}
		for _, ga := range a.Value.Group() {
			appendAttr(sb, g, ga)
		}
		return
	}
	key := a.Key
	if group != "" {
		key = group + "." + key
	}
	fmt.Fprintf(sb, " %s=%v", key, a.Value)
}
