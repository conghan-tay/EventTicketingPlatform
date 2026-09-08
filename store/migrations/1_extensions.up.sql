-- Extensions required by later migrations.
--
-- pg_trgm backs fuzzy/prefix matching on event titles alongside the tsvector index
-- (see docs/decisions.md D6: Postgres FTS now, OpenSearch behind a named trigger).
CREATE EXTENSION IF NOT EXISTS pg_trgm;
