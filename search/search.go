// Package search serves event discovery.
//
// It is a separate service so a heavy query cannot starve the booking connection pool
// (D8), and so the eventual move to OpenSearch is contained behind one interface (D6).
//
// Results are ordered by event start time, soonest first — not by relevance. Relevance
// ranking and stable keyset pagination pull in opposite directions: a rank is neither
// stable across writes nor indexable as a sort key, so paging by it either repeats or
// skips rows. Start-time ordering is stable, indexable and pageable. Relevance ranking
// is a deliberate deferral to the OpenSearch step, where the engine handles it
// properly; the tsvector already carries title/description weights for that day.
package search

import (
	"context"

	"encore.dev/beta/errs"
	"encore.dev/storage/sqldb"

	"encore.app/internal/clock"
)

var db = sqldb.Named("ticketing")

//encore:service
type Service struct {
	index SearchIndex
	clock clock.Clock
}

func initService() (*Service, error) {
	return &Service{
		index: &PostgresIndex{db: db},
		clock: clock.Default(),
	}, nil
}

// Search finds events on sale.
//
//encore:api public method=GET path=/v1/search
func (s *Service) Search(ctx context.Context, p *Params) (*Results, error) {
	q, err := p.parse()
	if err != nil {
		return nil, &errs.Error{Code: errs.InvalidArgument, Message: err.Error()}
	}
	return s.index.Search(ctx, q)
}
