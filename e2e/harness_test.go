//go:build e2e

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// H2: reset, seed and clock control work. Everything downstream depends on these, so
// they are proven before any feature test relies on them.
func TestHarnessReset(t *testing.T) {
	h := NewHarness(t)

	// Table existence is asserted in the catalog tests, once migrations create them.
	// Here we only prove that whatever tables exist come back empty.
	for table, n := range h.TableCounts() {
		assert.Zero(t, n, "table %q should be empty after reset", table)
	}
}

func TestHarnessClockSetAndAdvance(t *testing.T) {
	h := NewHarness(t)

	frozen := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	got := h.SetClock(frozen)
	require.True(t, got.Equal(frozen), "expected clock at %s, got %s", frozen, got)

	// Read it back rather than trusting the write, and confirm it does not drift.
	assert.True(t, h.ClockNow().Equal(frozen))
	time.Sleep(50 * time.Millisecond)
	assert.True(t, h.ClockNow().Equal(frozen), "frozen clock must not drift")

	after := h.AdvanceClock(11 * time.Minute)
	want := frozen.Add(11 * time.Minute)
	require.True(t, after.Equal(want), "expected %s, got %s", want, after)
	assert.True(t, h.ClockNow().Equal(want))
}

// Reset must also release the clock, or a frozen time would leak into later tests.
func TestHarnessResetReleasesClock(t *testing.T) {
	h := NewHarness(t)
	h.SetClock(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))

	h.Reset()
	assert.WithinDuration(t, time.Now().UTC(), h.ClockNow(), 5*time.Second,
		"reset must return the clock to real time")
}

// The clock only moves forward. Allowing it backwards would let a test fabricate a
// state the real system can never reach.
func TestHarnessClockRejectsNonPositiveAdvance(t *testing.T) {
	h := NewHarness(t)

	for _, secs := range []float64{0, -60} {
		resp, err := h.Post("/_test/clock/advance", map[string]any{"seconds": secs})
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.Status,
			"advancing by %v should be rejected, got %d: %s", secs, resp.Status, resp.Body)
	}
}

func TestHarnessClockSetRequiresTime(t *testing.T) {
	h := NewHarness(t)

	resp, err := h.Post("/_test/clock/set", map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
}
