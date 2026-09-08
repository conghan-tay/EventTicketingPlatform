// Package clock provides an injectable time source.
//
// Business logic must never call time.Now() directly (decision D11). Hold expiry,
// TTLs and reaping are all time-dependent, and tests need to control time to stay
// deterministic instead of sleeping.
//
// Services receive a Clock through their service struct so they stay unit-testable.
// The value they receive comes from Default(), which is controllable in local and
// test environments and real everywhere else.
package clock

import (
	"sync"
	"time"

	"encore.dev"
)

// Clock is a source of the current time.
type Clock interface {
	Now() time.Time
}

// Real returns the actual wall-clock time.
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

// Controllable is a Clock whose time can be set and advanced, so time-dependent
// behaviour can be exercised without sleeps.
//
// An untouched Controllable tracks real time, so it behaves exactly like Real until
// a test deliberately manipulates it.
type Controllable struct {
	mu     sync.RWMutex
	offset time.Duration
	frozen *time.Time
}

func NewControllable() *Controllable { return &Controllable{} }

func (c *Controllable) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.frozen != nil {
		return c.frozen.Add(c.offset).UTC()
	}
	return time.Now().Add(c.offset).UTC()
}

// Set freezes the clock at t. Subsequent Advance calls move forward from t.
func (c *Controllable) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tt := t.UTC()
	c.frozen = &tt
	c.offset = 0
}

// Advance moves the clock forward by d, whether or not it is frozen.
func (c *Controllable) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// Reset returns the clock to tracking real time.
func (c *Controllable) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen = nil
	c.offset = 0
}

var (
	once         sync.Once
	def          Clock
	controllable *Controllable
)

// initDefault decides once whether this process gets a controllable clock.
//
// It is lazy rather than done in a package initialiser because encore.Meta() is not
// guaranteed to be populated before the Encore runtime has started.
func initDefault() {
	once.Do(func() {
		if IsTimeControllableEnv() {
			controllable = NewControllable()
			def = controllable
			return
		}
		def = Real{}
	})
}

// Default returns the process-wide clock. In local and test environments this is a
// Controllable shared by all services in the process, which is what lets the
// testsupport service advance time on behalf of the whole app.
func Default() Clock {
	initDefault()
	return def
}

// Testable returns the controllable clock, or false in environments where time
// manipulation is not permitted.
func Testable() (*Controllable, bool) {
	initDefault()
	if controllable == nil {
		return nil, false
	}
	return controllable, true
}

// IsTimeControllableEnv reports whether this environment may have its clock
// manipulated: unit tests, and locally-running development environments.
func IsTimeControllableEnv() bool {
	env := encore.Meta().Environment
	return TimeControllable(env.Type, env.Cloud)
}

// TimeControllable is the pure decision behind IsTimeControllableEnv, separated so
// the guard can be unit-tested across the full environment matrix. Reading
// encore.Meta() directly would make that untestable, since a test cannot change the
// environment it runs in.
//
// Note that encore.EnvLocal is deprecated and no longer returned by the runtime; a
// locally running app reports EnvDevelopment combined with CloudLocal. Guarding on
// EnvLocal would therefore reject actual local development.
func TimeControllable(envType encore.EnvironmentType, cloud encore.CloudProvider) bool {
	switch envType {
	case encore.EnvTest:
		return true
	case encore.EnvDevelopment:
		// Cloud-hosted development environments are shared and must not be
		// time-manipulable; only a genuinely local run qualifies.
		return cloud == encore.CloudLocal
	default:
		// Production and ephemeral/preview environments never qualify.
		return false
	}
}
