-- Search index.
--
-- search_vector is a GENERATED column, so the index is updated inside the same
-- transaction as the write. That means no indexing pipeline, no indexing lag and
-- nothing to rebuild, which is the main practical reason to start on Postgres FTS
-- rather than an external engine (docs/decisions.md D6).
ALTER TABLE events
    ADD COLUMN search_vector tsvector
    GENERATED ALWAYS AS (
        -- Weighting: a title match is worth more than a description match. Unused
        -- today (results are ordered by start time) but stored now so relevance
        -- ranking needs no backfill.
        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(description, '')), 'B')
    ) STORED;

CREATE INDEX events_search_vector_idx ON events USING GIN (search_vector);

-- Trigram index on the title, supporting partial-word and typo-tolerant matching
-- that stemming alone does not cover ("trav" finding "Traviata").
CREATE INDEX events_title_trgm_idx ON events USING GIN (title gin_trgm_ops);

CREATE INDEX events_category_idx ON events (category, starts_at, event_id)
    WHERE status = 'ON_SALE';

-- Supports the bounding-box prefilter applied before the exact haversine distance.
CREATE INDEX venues_latlon_idx ON venues (latitude, longitude)
    WHERE latitude IS NOT NULL AND longitude IS NOT NULL;
