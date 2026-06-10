package logging

import (
	"io"
	"log/slog"
	"testing"
)

func newTestLogger(level slog.Level, capacity int) (*slog.Logger, *Ring, *slog.LevelVar) {
	lv := new(slog.LevelVar)
	lv.Set(level)
	ring := NewRing(capacity)
	base := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: lv})
	return slog.New(NewHandler(base, ring)), ring, lv
}

func TestRingCapturesAndWraps(t *testing.T) {
	log, ring, _ := newTestLogger(slog.LevelInfo, 4)
	for i := 0; i < 6; i++ {
		log.Info("msg", "i", i)
	}
	got := ring.Snapshot(slog.LevelDebug, 0)
	if len(got) != 4 {
		t.Fatalf("wrapped ring should hold 4, got %d", len(got))
	}
	// Oldest-first: entries 2..5 survive.
	if got[0].Attrs != "i=2" || got[3].Attrs != "i=5" {
		t.Fatalf("wrong wrap order: first=%q last=%q", got[0].Attrs, got[3].Attrs)
	}
}

func TestLevelGateAndLiveChange(t *testing.T) {
	log, ring, lv := newTestLogger(slog.LevelInfo, 16)
	log.Debug("hidden")
	log.Info("shown")
	if n := len(ring.Snapshot(slog.LevelDebug, 0)); n != 1 {
		t.Fatalf("debug record captured despite info level: %d entries", n)
	}
	lv.Set(slog.LevelDebug) // live verbosity bump, as the settings page does
	log.Debug("now visible", "k", "v")
	got := ring.Snapshot(slog.LevelDebug, 0)
	if len(got) != 2 || got[1].Level != "DEBUG" || got[1].Attrs != "k=v" {
		t.Fatalf("live level change not applied: %+v", got)
	}
}

func TestSnapshotFilterAndLimit(t *testing.T) {
	log, ring, _ := newTestLogger(slog.LevelDebug, 32)
	log.Info("a")
	log.Warn("b")
	log.Error("c")
	log.Info("d")
	if got := ring.Snapshot(slog.LevelWarn, 0); len(got) != 2 {
		t.Fatalf("warn filter: want 2, got %d", len(got))
	}
	got := ring.Snapshot(slog.LevelDebug, 2)
	if len(got) != 2 || got[0].Msg != "c" || got[1].Msg != "d" {
		t.Fatalf("limit should keep most-recent: %+v", got)
	}
}

func TestWithAttrsAndGroups(t *testing.T) {
	log, ring, _ := newTestLogger(slog.LevelInfo, 8)
	log.With("svc", "qbit").WithGroup("req").Info("hit", "path", "/x")
	got := ring.Snapshot(slog.LevelDebug, 0)
	if len(got) != 1 || got[0].Attrs != "svc=qbit req.path=/x" {
		t.Fatalf("attr flattening wrong: %+v", got)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"": slog.LevelInfo, "debug": slog.LevelDebug, "INFO": slog.LevelInfo,
		"Warn": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Fatalf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("loud"); err == nil {
		t.Fatalf("ParseLevel should reject unknown level")
	}
}
