// Package organizer handles event authoring: venues, seat topology, events, and
// publishing (which materialises inventory).
//
// Its write patterns are unrelated to attendee traffic, which is why it is a separate
// service from catalog and search (D8).
package organizer

import (
	"strings"

	"encore.dev/beta/auth"
	"encore.dev/beta/errs"
	"encore.dev/storage/sqldb"

	"encore.app/internal/clock"
)

var db = sqldb.Named("ticketing")

//encore:service
type Service struct {
	// Injected rather than calling time.Now() directly (D11), so created_at and the
	// on-sale window are deterministic under test.
	clock clock.Clock
}

func initService() (*Service, error) {
	return &Service{clock: clock.Default()}, nil
}

// callerID returns the authenticated organizer. Identity comes only from the verified
// auth context; a user id in a request body is never trusted.
func callerID() (string, error) {
	uid, ok := auth.UserID()
	if !ok {
		return "", &errs.Error{Code: errs.Unauthenticated, Message: "authentication required"}
	}
	return string(uid), nil
}

func badRequest(msg string) error {
	return &errs.Error{Code: errs.InvalidArgument, Message: msg}
}

func trimmed(s string) string { return strings.TrimSpace(s) }
