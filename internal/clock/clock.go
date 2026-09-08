// Package clock provides an injectable time source.
//
// Business logic must never call time.Now() directly (decision D11). Hold expiry,
// TTLs and reaping are all time-dependent, and tests need to control time to stay
// deterministic instead of sleeping.
package clock

import (
	"sync"
	"time"
)

// Clock is a source of the current time.
type Clock interface {
	Now() time.Time
}

// Real returns the actual wall-clock time.
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

// Controllable is a Clock whose time can be set and advanced. It is used in local
// and test environments so time-dependent behaviour can be exercised without sleeps.
//
// A zero offset means it tracks real time, so a Controllable that is never touched
// behaves exactly like Real.
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

// Advance moves the clock forward by d. It works whether or not the clock is frozen.
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
