//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"
)

const healthTimeout = 60 * time.Second

// TestMain fails the whole suite fast with a clear message if the app is not running,
// rather than letting every test fail with an opaque connection error.
func TestMain(m *testing.M) {
	if err := NewClient().WaitForHealth(healthTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "\nE2E suite requires a running app at %s\n"+
			"Start it with `encore run`, or run the whole suite with `make e2e`.\n\n  %v\n\n",
			BaseURL(), err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
