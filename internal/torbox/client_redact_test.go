package torbox_test

import (
	"testing"

	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/torbox"
	"github.com/stretchr/testify/require"
)

// TestRequestDL_TransportError_DoesNotLeakKey guards the redaction fix: requestdl
// carries the API key as a `token` query param, and a transport-level failure
// yields a *url.Error whose default string includes the full URL. The wrapped
// error must never expose the key.
func TestRequestDL_TransportError_DoesNotLeakKey(t *testing.T) {
	const secret = "SUPER-SECRET-KEY-do-not-leak-12345"
	// Port 1 refuses immediately, forcing the *url.Error transport path.
	c := torbox.NewForTest(config.TorBox{APIKey: secret, APIBase: "http://127.0.0.1:1"})

	_, err := c.RequestDL(7654321, 0)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret,
		"the API key must never appear in a transport error string")
}
