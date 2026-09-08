// Package store owns the single PostgreSQL database shared by the ticketing services.
//
// One database, not one per service, is deliberate. Publishing an event must
// atomically mark the event ON_SALE and materialise one ticket per seat, so events
// and tickets have to be transactable together. Service boundaries here are logical
// (enforced in code and reviewed), while the underlying Postgres is single-writer
// with read replicas, exactly as described in docs/system-design.md.
//
// Other services obtain a handle with sqldb.Named("ticketing").
package store

import (
	"context"
	"strings"

	"encore.dev/storage/sqldb"
)

// DB is the ticketing database. Table ownership by service:
//
//	organizer → venues, seats, events, price_tiers
//	booking   → tickets, holds, bookings   (the only writer of inventory)
//	payments  → payments
//	catalog   → none (derived reads only)
//	search    → none (derived reads only)
var DB = sqldb.NewDatabase("ticketing", sqldb.DatabaseConfig{
	Migrations: "./migrations",
})

// Stats is a private endpoint that reports table counts. It exists so this package
// is recognised as an Encore service that owns the database, and it is genuinely
// useful when diagnosing a failing test run.
type Stats struct {
	Tables map[string]int64 `json:"tables"`
}

//encore:api private method=GET path=/internal/store/stats
func GetStats(ctx context.Context) (*Stats, error) {
	names, err := tableNames(ctx)
	if err != nil {
		return nil, err
	}

	out := &Stats{Tables: make(map[string]int64, len(names))}
	for _, name := range names {
		var n int64
		// Table names come from the catalog, never from user input, so interpolating
		// the quoted identifier here cannot be an injection vector.
		if err := DB.QueryRow(ctx, `SELECT count(*) FROM "`+name+`"`).Scan(&n); err != nil {
			return nil, err
		}
		out.Tables[name] = n
	}
	return out, nil
}

// tableNames lists the application tables in the public schema, excluding the
// migration bookkeeping table.
//
// Reading the catalog rather than hardcoding a list means TruncateAll keeps working
// as later migrations add tables, with no maintenance.
func tableNames(ctx context.Context) ([]string, error) {
	rows, err := DB.Query(ctx, `
		SELECT tablename
		  FROM pg_tables
		 WHERE schemaname = 'public'
		   AND tablename NOT LIKE 'schema_migrations%'
		 ORDER BY tablename
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// TruncateAll empties every application table. Used only by the testsupport service,
// which enforces the environment guard before calling it.
func TruncateAll(ctx context.Context) error {
	names, err := tableNames(ctx)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}

	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, `"`+n+`"`)
	}

	// One statement so foreign keys never transiently break, and RESTART IDENTITY so
	// generated ids are stable across tests.
	stmt := "TRUNCATE TABLE " + strings.Join(quoted, ", ") + " RESTART IDENTITY CASCADE"
	_, err = DB.Exec(ctx, stmt)
	return err
}
