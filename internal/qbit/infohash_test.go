package qbit

import (
	"encoding/base32"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const bbbHex = "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c" // Big Buck Bunny btih

// bbbBase32 is bbbHex re-encoded as a 32-char base32 infohash (computed so the
// hex<->base32 pair is always self-consistent, never a hand-typed guess).
func bbbBase32(t *testing.T) string {
	t.Helper()
	raw, err := hex.DecodeString(bbbHex)
	require.NoError(t, err)
	return base32.StdEncoding.EncodeToString(raw) // 32 chars, uppercase, unpadded
}

// TestNormalizeInfohash covers the traversal guard (docs/15 §4.C) + base32
// support (§4.E): hex passes through lowercased, base32 converts to 40-hex, and
// anything that is not a real infohash — crucially a path-traversal payload —
// is rejected (returns "").
func TestNormalizeInfohash(t *testing.T) {
	b32 := bbbBase32(t)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"hex lowercase", bbbHex, bbbHex},
		{"hex uppercase normalizes", strings.ToUpper(bbbHex), bbbHex},
		{"base32 converts to hex", b32, bbbHex},
		{"base32 lowercase converts", strings.ToLower(b32), bbbHex},
		{"empty rejected", "", ""},
		{"short hex rejected", "abc123", ""},
		{"40 non-hex rejected", strings.Repeat("z", 40), ""},
		{"traversal payload rejected", "../../../../etc/passwd/aaaaaaaaaaaaaaaa", ""},
		{"slash in 40 rejected", "dd8255ecdc7ca55fb0bbf81323d87062db1f6d/c", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, normalizeInfohash(tt.in))
		})
	}
}

// TestIsInfohash asserts the 40-lowercase-hex chokepoint used right before the
// hash becomes the symlink target segment.
func TestIsInfohash(t *testing.T) {
	require.True(t, isInfohash("dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c"))
	require.False(t, isInfohash(""))
	require.False(t, isInfohash("DD8255ECDC7CA55FB0BBF81323D87062DB1F6D1C")) // uppercase not allowed here
	require.False(t, isInfohash("../../etc"))
	require.False(t, isInfohash("dd8255ecdc7ca55fb0bbf81323d87062db1f6d1cX"))
}

// TestParseMagnet_NormalizesAndDecodes covers hex/base32 xt extraction and the
// net/url-based dn decode (§4.E), plus rejection of a traversal xt.
func TestParseMagnet_NormalizesAndDecodes(t *testing.T) {
	h, n := parseMagnet("magnet:?xt=urn:btih:" + bbbHex + "&dn=Big%20Buck%20Bunny")
	require.Equal(t, bbbHex, h)
	require.Equal(t, "Big Buck Bunny", n)

	// base32 xt normalizes to hex.
	h2, _ := parseMagnet("magnet:?xt=urn:btih:" + bbbBase32(t))
	require.Equal(t, bbbHex, h2)

	// Well-formed dn: + decodes to space (query-style), %20 too.
	_, nOK := parseMagnet("magnet:?xt=urn:btih:" + bbbHex + "&dn=a+b%20c")
	require.Equal(t, "a b c", nOK)

	// Malformed percent-encoding: QueryUnescape errors, so urlDecode returns the
	// raw value verbatim (graceful — no panic, no dropped grab).
	_, nBad := parseMagnet("magnet:?xt=urn:btih:" + bbbHex + "&dn=a+b%ZZ")
	require.Equal(t, "a+b%ZZ", nBad)

	// traversal payload as xt yields no usable hash.
	h4, _ := parseMagnet("magnet:?xt=urn:btih:../../../../etc&dn=x")
	require.Equal(t, "", h4)
}
