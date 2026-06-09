// export_test.go exposes internal constructors for the test package only.
// It is compiled exclusively as part of the test binary.
package torbox

import "github.com/rushp4000/lazarr/internal/config"

// NewForTest constructs a *client with the base URL taken from cfg.APIBase so
// tests can point at an httptest.Server without touching production code paths.
func NewForTest(cfg config.TorBox, opts ...option) Client {
	return newClient(cfg, opts...)
}
