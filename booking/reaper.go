package booking

import (
	"context"
	"time"

	"encore.dev/cron"
	"encore.dev/rlog"

	"encore.app/internal/clock"
)

// reapBatchSize bounds one sweep so a large backlog cannot hold locks for long or
// produce an unbounded transaction.
const reapBatchSize = 1000

// maxReapBatches bounds total work per invocation, so the endpoint always returns
// promptly and cron never overlaps with itself indefinitely.
const maxReapBatches = 50

type ReapResult struct {
	TicketsReleased int64 `json:"tickets_released"`
	HoldsExpired    int64 `json:"holds_expired"`
	// Truncated reports that the batch limit was hit and work remains, so an operator
	// can tell "nothing to do" apart from "falling behind".
	Truncated bool `json:"truncated"`
}

// ReapExpiredHolds returns seats from lapsed leases to inventory.
//
// Registered as a cron job below, but Encore cron does not fire locally or in preview
// environments (D10), so it is also callable directly. Being an endpoint rather than
// an in-process timer is what makes expiry testable without sleeping.
//
// It is idempotent, as Encore requires of cron endpoints: running it twice releases
// nothing extra, and running it concurrently is safe.
//
//encore:api private method=POST path=/internal/holds/reap
func ReapExpiredHolds(ctx context.Context) (*ReapResult, error) {
	// A package-level function rather than a service-struct method, because cron
	// requires an endpoint taking only a context. The sweep logic lives on Service so
	// it can be unit-tested with a controlled clock; this is just the entry point.
	return (&Service{clock: clock.Default()}).reap(ctx)
}

func (s *Service) reap(ctx context.Context) (*ReapResult, error) {
	// One timestamp for the whole sweep, from the injected clock rather than SQL
	// now(), so a test can advance time and see a deterministic result.
	now := s.clock.Now()
	out := &ReapResult{}

	for i := range maxReapBatches {
		released, err := s.reapBatch(ctx, now)
		if err != nil {
			return nil, err
		}
		out.TicketsReleased += released
		if released < reapBatchSize {
			break
		}
		if i == maxReapBatches-1 {
			out.Truncated = true
		}
	}

	// Mark lapsed holds expired. Separate from the ticket release and safe to repeat:
	// the predicate only matches rows still claiming to be ACTIVE.
	res, err := db.Exec(ctx, `
		UPDATE holds SET status = 'EXPIRED'
		 WHERE status = 'ACTIVE' AND expires_at <= $1
	`, now)
	if err != nil {
		return nil, err
	}
	out.HoldsExpired = res.RowsAffected()

	if out.TicketsReleased > 0 {
		mTicketsReaped.Add(uint64(out.TicketsReleased))
	}

	if out.TicketsReleased > 0 || out.HoldsExpired > 0 {
		rlog.Info("reaped expired holds",
			"tickets_released", out.TicketsReleased,
			"holds_expired", out.HoldsExpired,
			"truncated", out.Truncated)
	}
	return out, nil
}

// reapBatch releases one bounded batch of lapsed tickets.
//
// Two properties make this safe:
//
//   - FOR UPDATE SKIP LOCKED: several reapers, or a reaper running alongside a live
//     claim, never block each other. A row being worked on elsewhere is simply left
//     for the next sweep.
//   - status = 'HELD' appears in both the selection and the update predicate, so a
//     SOLD ticket can never be released. That is the invariant this function must
//     not violate under any interleaving.
func (s *Service) reapBatch(ctx context.Context, now time.Time) (int64, error) {
	res, err := db.Exec(ctx, `
		WITH expired AS (
			SELECT event_id, ticket_id
			  FROM tickets
			 WHERE status = 'HELD' AND hold_expires_at <= $1
			 ORDER BY ticket_id
			 LIMIT $2
			   FOR UPDATE SKIP LOCKED
		)
		UPDATE tickets t
		   SET status = 'AVAILABLE', hold_id = NULL, hold_expires_at = NULL
		  FROM expired e
		 WHERE t.event_id = e.event_id
		   AND t.ticket_id = e.ticket_id
		   AND t.status = 'HELD'
	`, now, reapBatchSize)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// Cron runs the sweep in deployed environments. Locally and in preview environments
// Encore does not fire cron jobs, which is why the endpoint above exists.
//
// One minute divides 24 hours evenly, as Encore requires of the Every field.
var _ = cron.NewJob("reap-expired-holds", cron.JobConfig{
	Title:    "Release seats from expired ticket holds",
	Endpoint: ReapExpiredHolds,
	Every:    1 * cron.Minute,
})
