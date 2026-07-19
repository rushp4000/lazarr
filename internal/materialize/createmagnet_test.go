package materialize

import (
	"testing"

	"github.com/rushp4000/lazarr/internal/catalog"
)

func TestCreateMagnet(t *testing.T) {
	const hash = "23ec1ed6bd7a7417564131922cefd344d69814d9"
	cases := []struct {
		name string
		rel  *catalog.Release
		want string
	}{
		{
			name: "torrent-file grab (no magnet) synthesizes bare-btih from hash",
			rel:  &catalog.Release{Hash: hash, Magnet: ""},
			want: "magnet:?xt=urn:btih:" + hash,
		},
		{
			name: "whitespace-only magnet is treated as absent",
			rel:  &catalog.Release{Hash: hash, Magnet: "   \n"},
			want: "magnet:?xt=urn:btih:" + hash,
		},
		{
			name: "non-magnet url (torznab download link) is not submitted verbatim",
			rel:  &catalog.Release{Hash: hash, Magnet: "https://nyaa.si/download/1.torrent"},
			want: "magnet:?xt=urn:btih:" + hash,
		},
		{
			name: "real magnet is preserved (tracker-rich, better for uncached)",
			rel:  &catalog.Release{Hash: hash, Magnet: "magnet:?xt=urn:btih:" + hash + "&tr=udp://tracker"},
			want: "magnet:?xt=urn:btih:" + hash + "&tr=udp://tracker",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := createMagnet(tc.rel); got != tc.want {
				t.Fatalf("createMagnet() = %q, want %q", got, tc.want)
			}
		})
	}
}
