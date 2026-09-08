package clock

import (
	"testing"
	"time"

	"encore.dev"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// S1: the environment guard. A regression here would either break local development
// or, far worse, expose time manipulation and the development auth handler in a real
// environment. Neither failure is visible from an E2E test, which cannot change the
// environment it runs in.
func TestTimeControllable(t *testing.T) {
	cases := []struct {
		name    string
		envType encore.EnvironmentType
		cloud   encore.CloudProvider
		want    bool
	}{
		{"unit test", encore.EnvTest, encore.CloudLocal, true},
		{"local run reports development+local", encore.EnvDevelopment, encore.CloudLocal, true},
		{"cloud-hosted development is shared", encore.EnvDevelopment, encore.CloudAWS, false},
		{"production", encore.EnvProduction, encore.CloudAWS, false},
		{"production on local cloud is still production", encore.EnvProduction, encore.CloudLocal, false},
		{"ephemeral preview env", encore.EnvEphemeral, encore.CloudGCP, false},
		{"unknown future env type fails closed", encore.EnvironmentType("something-new"), encore.CloudLocal, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, TimeControllable(tc.envType, tc.cloud))
		})
	}
}

func TestRealClockIsUTC(t *testing.T) {
	assert.Equal(t, time.UTC, Real{}.Now().Location())
}

// An untouched Controllable must behave like the real clock, so merely wiring one in
// cannot change behaviour.
func TestControllableTracksRealTimeUntilTouched(t *testing.T) {
	c := NewControllable()
	assert.WithinDuration(t, time.Now().UTC(), c.Now(), 2*time.Second)
}

func TestControllableSetFreezesTime(t *testing.T) {
	c := NewControllable()
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	c.Set(at)

	require.Equal(t, at, c.Now())
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, at, c.Now(), "a frozen clock must not drift")
}

func TestControllableAdvance(t *testing.T) {
	c := NewControllable()
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	c.Set(at)

	c.Advance(10 * time.Minute)
	assert.Equal(t, at.Add(10*time.Minute), c.Now())

	// Advances accumulate; this is what lets a test step past several TTLs in turn.
	c.Advance(5 * time.Minute)
	assert.Equal(t, at.Add(15*time.Minute), c.Now())
}

func TestControllableAdvanceWorksWhenNotFrozen(t *testing.T) {
	c := NewControllable()
	before := c.Now()
	c.Advance(time.Hour)
	assert.True(t, c.Now().Sub(before) >= time.Hour)
}

func TestControllableReset(t *testing.T) {
	c := NewControllable()
	c.Set(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	c.Advance(24 * time.Hour)

	c.Reset()
	assert.WithinDuration(t, time.Now().UTC(), c.Now(), 2*time.Second,
		"Reset must return the clock to real time so state does not leak between tests")
}
