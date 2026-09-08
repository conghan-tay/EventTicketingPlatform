//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Harness bundles a client with the helpers tests need. Construct it with NewHarness,
// which resets state so each test starts from a known baseline.
type Harness struct {
	*Client
	t *testing.T
}

// NewHarness resets all application state and returns a harness for the test.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	h := &Harness{Client: NewClient(), t: t}
	h.Reset()
	return h
}

// Reset empties every table and returns the clock to real time.
func (h *Harness) Reset() {
	h.t.Helper()
	resp, err := h.Post("/_test/reset", nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "reset failed: %s", resp.Body)
}

// SetClock freezes application time at the given instant.
func (h *Harness) SetClock(at time.Time) time.Time {
	h.t.Helper()
	resp, err := h.Post("/_test/clock/set", map[string]any{"now": at})
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "set clock failed: %s", resp.Body)

	var out struct {
		Now time.Time `json:"now"`
	}
	require.NoError(h.t, resp.DecodeInto(&out))
	return out.Now
}

// AdvanceClock moves application time forward, so expiry can be tested without sleeps.
func (h *Harness) AdvanceClock(d time.Duration) time.Time {
	h.t.Helper()
	resp, err := h.Post("/_test/clock/advance", map[string]any{"seconds": d.Seconds()})
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "advance clock failed: %s", resp.Body)

	var out struct {
		Now time.Time `json:"now"`
	}
	require.NoError(h.t, resp.DecodeInto(&out))
	return out.Now
}

// ClockNow reads application time, so a test can assert the clock really moved
// rather than trusting the advance call.
func (h *Harness) ClockNow() time.Time {
	h.t.Helper()
	resp, err := h.Get("/_test/clock/now", nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "clock now failed: %s", resp.Body)

	var out struct {
		Now time.Time `json:"now"`
	}
	require.NoError(h.t, resp.DecodeInto(&out))
	return out.Now
}

// TableCounts reports row counts per table, used to assert that reset really emptied
// state and that publishing materialised the expected number of tickets.
func (h *Harness) TableCounts() map[string]int64 {
	h.t.Helper()
	resp, err := h.Get("/_test/stats", nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "stats failed: %s", resp.Body)

	var out struct {
		Tables map[string]int64 `json:"tables"`
	}
	require.NoError(h.t, resp.DecodeInto(&out))
	return out.Tables
}

// Query builds a query string, keeping search tests readable.
func Query(pairs map[string]string) url.Values {
	v := url.Values{}
	for k, val := range pairs {
		if val != "" {
			v.Set(k, val)
		}
	}
	return v
}
