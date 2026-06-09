package qbit

import (
	"crypto/sha1" //nolint:gosec // sha1 is required by the BitTorrent protocol
	"fmt"
)

// sha1Sum computes the SHA-1 digest of b (required by the BitTorrent infohash spec).
func sha1Sum(b []byte) []byte {
	h := sha1.Sum(b) //nolint:gosec
	return h[:]
}

// findInfoDict locates the raw bytes of the "info" value in a bencode-encoded torrent.
// Returns start and end byte offsets (end is exclusive) and whether it was found.
//
// Bencode format reference:
//
//	string  → <length>:<bytes>
//	integer → i<decimal>e
//	list    → l<items>e
//	dict    → d<key><value>...e  (keys are bencode strings, sorted)
func findInfoDict(buf []byte) (start, end int, found bool) {
	// Walk the top-level dict looking for key "4:info".
	pos := 0
	if pos >= len(buf) || buf[pos] != 'd' {
		return 0, 0, false
	}
	pos++ // skip 'd'

	for pos < len(buf) && buf[pos] != 'e' {
		// Read key (bencode string).
		keyStart := pos
		keyEnd, ok := bencodeStringEnd(buf, pos)
		if !ok {
			return 0, 0, false
		}
		keyBytes := bencodeStringValue(buf, keyStart)
		pos = keyEnd

		// Value starts at pos.
		valStart := pos
		valEnd, ok2 := bencodeSkip(buf, pos)
		if !ok2 {
			return 0, 0, false
		}

		if string(keyBytes) == "info" {
			return valStart, valEnd, true
		}
		pos = valEnd
	}
	return 0, 0, false
}

// bencodeStringEnd returns the position AFTER a bencode string at buf[pos].
func bencodeStringEnd(buf []byte, pos int) (end int, ok bool) {
	colon := -1
	for i := pos; i < len(buf); i++ {
		if buf[i] == ':' {
			colon = i
			break
		}
		if buf[i] < '0' || buf[i] > '9' {
			return 0, false
		}
	}
	if colon < 0 {
		return 0, false
	}
	var length int
	if _, err := fmt.Sscanf(string(buf[pos:colon]), "%d", &length); err != nil {
		return 0, false
	}
	// Overflow-safe bound: compare length against the remaining buffer rather
	// than computing colon+1+length (which can overflow for a hostile length
	// near MaxInt and wrap negative, defeating an `end > len(buf)` check).
	if length < 0 || length > len(buf)-(colon+1) {
		return 0, false
	}
	return colon + 1 + length, true
}

// bencodeStringValue returns the raw string bytes of a bencode string at buf[pos].
func bencodeStringValue(buf []byte, pos int) []byte {
	colon := -1
	for i := pos; i < len(buf); i++ {
		if buf[i] == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return nil
	}
	var length int
	if _, err := fmt.Sscanf(string(buf[pos:colon]), "%d", &length); err != nil {
		return nil
	}
	start := colon + 1
	// Overflow-safe bound (see bencodeStringEnd).
	if length < 0 || length > len(buf)-start {
		return nil
	}
	return buf[start : start+length]
}

// maxBencodeDepth bounds nested list/dict recursion so a hostile .torrent with
// deeply nested containers cannot exhaust the goroutine stack (a fatal,
// unrecoverable crash). Real torrents nest only a few levels.
const maxBencodeDepth = 128

// bencodeSkip returns the position after a single bencode value at buf[pos].
func bencodeSkip(buf []byte, pos int) (end int, ok bool) {
	return bencodeSkipDepth(buf, pos, 0)
}

func bencodeSkipDepth(buf []byte, pos, depth int) (end int, ok bool) {
	if depth > maxBencodeDepth {
		return 0, false
	}
	if pos >= len(buf) {
		return 0, false
	}
	switch {
	case buf[pos] == 'i':
		// integer: i<decimal>e
		for i := pos + 1; i < len(buf); i++ {
			if buf[i] == 'e' {
				return i + 1, true
			}
		}
		return 0, false
	case buf[pos] == 'l':
		// list
		pos++
		for pos < len(buf) && buf[pos] != 'e' {
			pos, ok = bencodeSkipDepth(buf, pos, depth+1)
			if !ok {
				return 0, false
			}
		}
		if pos >= len(buf) {
			return 0, false
		}
		return pos + 1, true
	case buf[pos] == 'd':
		// dict
		pos++
		for pos < len(buf) && buf[pos] != 'e' {
			// key
			pos, ok = bencodeSkipDepth(buf, pos, depth+1)
			if !ok {
				return 0, false
			}
			// value
			pos, ok = bencodeSkipDepth(buf, pos, depth+1)
			if !ok {
				return 0, false
			}
		}
		if pos >= len(buf) {
			return 0, false
		}
		return pos + 1, true
	case buf[pos] >= '0' && buf[pos] <= '9':
		// string
		end, ok2 := bencodeStringEnd(buf, pos)
		return end, ok2
	default:
		return 0, false
	}
}

// bencodeFindString finds the value of a string key within a bencode dict (raw bytes).
func bencodeFindString(buf []byte, key string) string {
	pos := 0
	if pos >= len(buf) || buf[pos] != 'd' {
		return ""
	}
	pos++ // skip 'd'

	for pos < len(buf) && buf[pos] != 'e' {
		keyStart := pos
		keyEnd, ok := bencodeStringEnd(buf, pos)
		if !ok {
			return ""
		}
		keyBytes := bencodeStringValue(buf, keyStart)
		pos = keyEnd

		if string(keyBytes) == key {
			// Value is a string.
			v := bencodeStringValue(buf, pos)
			return string(v)
		}

		// Skip value.
		var ok2 bool
		pos, ok2 = bencodeSkip(buf, pos)
		if !ok2 {
			return ""
		}
	}
	return ""
}
