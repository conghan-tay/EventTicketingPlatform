// Package testsupport exposes state and time control to the test suite.
//
// Every endpoint here is guarded by requireControllableEnv, so the service is inert
// in any environment other than a unit test or a locally-running app. The guard is
// checked per request rather than at startup so it cannot be bypassed by a service
// that happens to initialise before the runtime metadata is populated.
package testsupport

import (
	"context"
	"time"

	"encore.dev/beta/errs"

	"encore.app/booking"
	"encore.app/catalog"
	"encore.app/internal/clock"
	"encore.app/payments"
	"encore.app/store"
)

// requireControllableEnv rejects the request unless this environment permits test
// control. See clock.IsTimeControllableEnv for how a local environment is detected.
func requireControllableEnv() error {
	if clock.IsTimeControllableEnv() {
		return nil
	}
	return &errs.Error{
		Code:    errs.PermissionDenied,
		Message: "testsupport is not available in this environment",
	}
}

type ResetResponse struct {
	Truncated bool `json:"truncated"`
}

// Reset empties every application table, returns the clock to real time, and restores
// the mock payment provider. Called between E2E tests so each starts from a known
// state — including the provider, or a scripted decline would leak into later tests.
//
//encore:api public method=POST path=/_test/reset
func Reset(ctx context.Context) (*ResetResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	if err := store.TruncateAll(ctx); err != nil {
		return nil, err
	}
	if c, ok := clock.Testable(); ok {
		c.Reset()
	}
	if _, err := payments.ResetTestProvider(ctx); err != nil {
		return nil, err
	}
	if _, err := catalog.ResetCacheStats(ctx); err != nil {
		return nil, err
	}
	return &ResetResponse{Truncated: true}, nil
}

type CacheStatsResponse struct {
	Hits     int64 `json:"hits"`
	Misses   int64 `json:"misses"`
	Errors   int64 `json:"errors"`
	Rebuilds int64 `json:"rebuilds"`
	Shed     int64 `json:"shed"`
}

// CacheStats exposes catalog cache counters, so a test can prove a repeat read was
// actually served from cache rather than assuming it.
//
//encore:api public method=GET path=/_test/cache-stats
func CacheStats(ctx context.Context) (*CacheStatsResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	s, err := catalog.GetCacheStats(ctx)
	if err != nil {
		return nil, err
	}
	return &CacheStatsResponse{
		Hits: s.Hits, Misses: s.Misses, Errors: s.Errors,
		Rebuilds: s.Rebuilds, Shed: s.Shed,
	}, nil
}

// RefreshAvailability drops an event's cached availability snapshot.
//
// Encore cache TTLs are real Redis expiries, so the injected clock cannot age them
// out. Rather than sleeping through the 5s TTL, tests drop the key explicitly.
//
//encore:api public method=POST path=/_test/refresh-availability/:eventID
func RefreshAvailability(ctx context.Context, eventID int64) (*catalog.RefreshResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	return catalog.RefreshAvailability(ctx, eventID)
}

// Integrity audits an event's inventory from the authoritative tables.
//
// Public here only so the load proof can assert on it; the underlying booking
// endpoint is private and the environment guard still applies.
//
//encore:api public method=GET path=/_test/integrity/:eventID
func Integrity(ctx context.Context, eventID int64) (*booking.Integrity, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	return booking.CheckIntegrity(ctx, eventID)
}

type ClockResponse struct {
	Now time.Time `json:"now"`
}

type SetClockRequest struct {
	Now time.Time `json:"now"`
}

// SetClock freezes application time at the given instant.
//
//encore:api public method=POST path=/_test/clock/set
func SetClock(ctx context.Context, req *SetClockRequest) (*ClockResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	c, ok := clock.Testable()
	if !ok {
		return nil, &errs.Error{Code: errs.FailedPrecondition, Message: "clock is not controllable"}
	}
	if req.Now.IsZero() {
		return nil, &errs.Error{Code: errs.InvalidArgument, Message: "now is required"}
	}
	c.Set(req.Now)
	return &ClockResponse{Now: c.Now()}, nil
}

type AdvanceClockRequest struct {
	// Seconds to move the clock forward. Fractional values are supported.
	Seconds float64 `json:"seconds"`
}

// AdvanceClock moves application time forward, so expiry behaviour can be tested
// without sleeping.
//
//encore:api public method=POST path=/_test/clock/advance
func AdvanceClock(ctx context.Context, req *AdvanceClockRequest) (*ClockResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	c, ok := clock.Testable()
	if !ok {
		return nil, &errs.Error{Code: errs.FailedPrecondition, Message: "clock is not controllable"}
	}
	if req.Seconds <= 0 {
		return nil, &errs.Error{
			Code:    errs.InvalidArgument,
			Message: "seconds must be positive; the clock only moves forward",
		}
	}
	c.Advance(time.Duration(req.Seconds * float64(time.Second)))
	return &ClockResponse{Now: c.Now()}, nil
}

// Now reports the current application time, so a test can assert the clock actually
// moved rather than trusting the advance call.
//
//encore:api public method=GET path=/_test/clock/now
func Now(ctx context.Context) (*ClockResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	return &ClockResponse{Now: clock.Default().Now()}, nil
}

type StatsResponse struct {
	Tables map[string]int64 `json:"tables"`
}

// Stats exposes per-table row counts to the test suite.
//
// store.GetStats is a private endpoint and therefore unreachable from outside the
// app, so this guarded public endpoint proxies it as a service-to-service call.
//
//encore:api public method=GET path=/_test/stats
func Stats(ctx context.Context) (*StatsResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	s, err := store.GetStats(ctx)
	if err != nil {
		return nil, err
	}
	return &StatsResponse{Tables: s.Tables}, nil
}

type PaymentBehaviourRequest struct {
	// Mode is one of succeed, decline, error, refund_fails.
	Mode string `json:"mode"`
}

type PaymentBehaviourResponse struct {
	Mode string `json:"mode"`
}

// SetPaymentBehaviour scripts the mock payment provider so the sad paths of the
// purchase saga are deterministic rather than dependent on a real gateway.
//
//encore:api public method=POST path=/_test/payments/behaviour
func SetPaymentBehaviour(ctx context.Context, req *PaymentBehaviourRequest) (*PaymentBehaviourResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	out, err := payments.SetTestBehaviour(ctx, &payments.SetBehaviourRequest{Mode: req.Mode})
	if err != nil {
		return nil, err
	}
	return &PaymentBehaviourResponse{Mode: out.Mode}, nil
}

type PaymentSummaryResponse struct {
	Charges int `json:"charges"`
	Refunds int `json:"refunds"`
}

// PaymentSummary reports provider call counts, so a test can prove a card was charged
// exactly once across a retry.
//
//encore:api public method=GET path=/_test/payments/summary
func PaymentSummary(ctx context.Context) (*PaymentSummaryResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	out, err := payments.TestSummary(ctx)
	if err != nil {
		return nil, err
	}
	return &PaymentSummaryResponse{Charges: out.Charges, Refunds: out.Refunds}, nil
}

type ReapResponse struct {
	TicketsReleased int64 `json:"tickets_released"`
	HoldsExpired    int64 `json:"holds_expired"`
}

// ReapHolds runs the expiry reaper on demand.
//
// Encore cron jobs do not fire locally or in preview environments (D10), so this
// guarded proxy is how tests drive expiry. It is also how an operator would force a
// sweep.
//
//encore:api public method=POST path=/_test/reap-holds
func ReapHolds(ctx context.Context) (*ReapResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	out, err := booking.ReapExpiredHolds(ctx)
	if err != nil {
		return nil, err
	}
	return &ReapResponse{
		TicketsReleased: out.TicketsReleased,
		HoldsExpired:    out.HoldsExpired,
	}, nil
}
