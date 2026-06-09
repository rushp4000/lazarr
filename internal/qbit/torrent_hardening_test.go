package qbit

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBencode_HostileLength_NoPanic guards the overflow-safe length bound: a
// bencode string claiming a MaxInt64 length must be rejected, not cause a
// slice-bounds panic via colon+1+length wrapping negative.
func TestBencode_HostileLength_NoPanic(t *testing.T) {
	hostile := []byte("d9223372036854775807:")
	require.NotPanics(t, func() {
		_, _, found := findInfoDict(hostile)
		require.False(t, found)
	})
}

// TestBencode_DeepNesting_NoStackOverflow guards the recursion depth cap: an
// info value nested far beyond maxBencodeDepth must fail cleanly rather than
// recurse without bound.
func TestBencode_DeepNesting_NoStackOverflow(t *testing.T) {
	depth := maxBencodeDepth + 50
	var b strings.Builder
	b.WriteString("d4:info")
	b.WriteString(strings.Repeat("l", depth))
	b.WriteString(strings.Repeat("e", depth))
	b.WriteByte('e')

	require.NotPanics(t, func() {
		_, _, found := findInfoDict([]byte(b.String()))
		require.False(t, found) // depth-limited skip → info value not resolved
	})
}

// TestParseTorrentFile_OversizedRejected guards the read cap.
func TestParseTorrentFile_OversizedRejected(t *testing.T) {
	big := bytes.Repeat([]byte("a"), maxTorrentBytes+10)
	_, _, err := parseTorrentFile(bytes.NewReader(big))
	require.Error(t, err)
}

// TestParseTorrentFile_Valid confirms a minimal well-formed torrent still parses.
func TestParseTorrentFile_Valid(t *testing.T) {
	data := []byte("d4:infod6:lengthi3e4:name3:fooee")
	hash, name, err := parseTorrentFile(bytes.NewReader(data))
	require.NoError(t, err)
	require.Len(t, hash, 40) // hex SHA-1
	require.Equal(t, "foo", name)
}
