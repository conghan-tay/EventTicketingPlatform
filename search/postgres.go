package search

import (
	"context"
	"time"

	"encore.dev/storage/sqldb"

	"encore.app/internal/cursor"
)

// PostgresIndex implements SearchIndex against the primary database using a tsvector
// GIN index plus a trigram index on the title.
type PostgresIndex struct {
	db *sqldb.Database
}

// searchSQL is a single static statement with every filter expressed as a nullable
// parameter.
//
// One static query rather than string concatenation means: no injection surface, one
// prepared-statement plan cache entry instead of one per filter combination, and the
// filter logic is visible in one place.
//
// Notable pieces:
//
//   - websearch_to_tsquery, not to_tsquery: it never errors on user input.
//     to_tsquery raises a syntax error on something as ordinary as an unbalanced
//     quote, which would turn a typo into a 500.
//   - The keyset predicate is a row comparison on (starts_at, event_id). Both columns
//     are needed because start times are not unique; comparing on starts_at alone
//     would skip or repeat events that begin at the same instant.
//   - The distance filter is a cheap bounding box (index-usable) followed by an exact
//     haversine check, so radius results are precise rather than square.
//   - LIMIT is $N+1: fetching one extra row is how has_more is determined without a
//     second COUNT query over the whole result set.
const searchSQL = `
WITH params AS (
    SELECT
        $1::text        AS q,
        $2::text        AS category,
        $3::timestamptz AS from_ts,
        $4::timestamptz AS to_ts,
        $5::float8      AS lat,
        $6::float8      AS lng,
        $7::float8      AS radius_km,
        $8::timestamptz AS after_starts_at,
        $9::bigint      AS after_event_id
),
matched AS (
    SELECT
        e.event_id, e.title, e.description, e.category, e.starts_at, e.ends_at,
        v.venue_id, v.name AS venue_name, v.city, v.country,
        CASE WHEN p.lat IS NULL THEN NULL ELSE
            6371.0 * 2 * asin(sqrt(
                power(sin(radians(v.latitude - p.lat) / 2), 2) +
                cos(radians(p.lat)) * cos(radians(v.latitude)) *
                power(sin(radians(v.longitude - p.lng) / 2), 2)
            ))
        END AS distance_km
      FROM events e
      JOIN venues v ON v.venue_id = e.venue_id
      CROSS JOIN params p
     WHERE e.status = 'ON_SALE'

       -- Free text: stemmed full-text match, or a trigram substring match on the
       -- title for partial words that stemming does not reach.
       AND (
            p.q IS NULL
         OR e.search_vector @@ websearch_to_tsquery('english', p.q)
         OR e.title ILIKE '%' || p.q || '%'
       )

       AND (p.category IS NULL OR e.category = p.category)
       AND (p.from_ts  IS NULL OR e.starts_at >= p.from_ts)
       AND (p.to_ts    IS NULL OR e.starts_at <= p.to_ts)

       -- Bounding box prefilter. A venue without coordinates is excluded, which is
       -- correct: its location is unknown, so it cannot be known to be in range.
       AND (
            p.lat IS NULL
         OR (
                v.latitude  IS NOT NULL
            AND v.longitude IS NOT NULL
            AND v.latitude  BETWEEN p.lat - (p.radius_km / 111.045)
                                AND p.lat + (p.radius_km / 111.045)
            AND v.longitude BETWEEN p.lng - (p.radius_km / (111.045 * greatest(cos(radians(p.lat)), 0.01)))
                                AND p.lng + (p.radius_km / (111.045 * greatest(cos(radians(p.lat)), 0.01)))
         )
       )

       -- Keyset pagination: strictly after the last row of the previous page.
       AND (
            p.after_event_id IS NULL
         OR (e.starts_at, e.event_id) > (p.after_starts_at, p.after_event_id)
       )
)
SELECT
    m.event_id, m.title, m.description, m.category, m.starts_at, m.ends_at,
    m.venue_id, m.venue_name, m.city, m.country, m.distance_km,
    coalesce(agg.min_price_cents, 0) AS min_price_cents,
    coalesce(agg.available, 0)       AS available
  FROM matched m
  CROSS JOIN params p
  LEFT JOIN LATERAL (
      SELECT min(pt.price_cents) AS min_price_cents,
             count(t.ticket_id) FILTER (WHERE t.status = 'AVAILABLE') AS available
        FROM price_tiers pt
        LEFT JOIN tickets t
               ON t.event_id = pt.event_id AND t.price_tier_id = pt.price_tier_id
       WHERE pt.event_id = m.event_id
  ) agg ON true
 -- Exact distance check, discarding the bounding box's corners.
 WHERE p.radius_km IS NULL OR m.distance_km <= p.radius_km
 ORDER BY m.starts_at, m.event_id
 LIMIT $10
`

func (idx *PostgresIndex) Search(ctx context.Context, q Query) (*Results, error) {
	// nilIfEmpty lets one static statement serve every filter combination.
	var text, category *string
	if q.Text != "" {
		text = &q.Text
	}
	if q.Category != "" {
		category = &q.Category
	}

	var lat, lng, radius *float64
	if q.Location != nil {
		lat, lng, radius = &q.Location.Lat, &q.Location.Lng, &q.Location.RadiusKM
	}

	var afterStarts *time.Time
	var afterID *int64
	if q.After != nil {
		afterStarts, afterID = &q.After.StartsAt, &q.After.EventID
	}

	// Fetch one more than requested to detect a further page without a second query.
	rows, err := idx.db.Query(ctx, searchSQL,
		text, category, q.From, q.To, lat, lng, radius, afterStarts, afterID, q.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &Results{Results: []Hit{}, Stale: true}
	for rows.Next() {
		var h Hit
		if err := rows.Scan(
			&h.EventID, &h.Title, &h.Description, &h.Category, &h.StartsAt, &h.EndsAt,
			&h.VenueID, &h.VenueName, &h.City, &h.Country, &h.DistanceKM,
			&h.MinPriceCents, &h.Available,
		); err != nil {
			return nil, err
		}
		out.Results = append(out.Results, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(out.Results) > q.Limit {
		// Drop the probe row; it belongs to the next page.
		out.Results = out.Results[:q.Limit]
		out.HasMore = true

		last := out.Results[len(out.Results)-1]
		next, err := cursor.Encode(cursor.Key{StartsAt: last.StartsAt, EventID: last.EventID})
		if err != nil {
			return nil, err
		}
		out.NextCursor = next
	}

	return out, nil
}
