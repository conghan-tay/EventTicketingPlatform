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
	"fmt"

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
//
// Individual partitions are excluded (relispartition). `tickets` is hash-partitioned,
// and including its partitions would double-count rows in the stats and list both
// parent and children in one TRUNCATE.
func tableNames(ctx context.Context) ([]string, error) {
	rows, err := DB.Query(ctx, `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public'
		   AND c.relkind IN ('r', 'p')       -- ordinary and partitioned tables
		   AND NOT c.relispartition          -- exclude individual partitions
		   AND c.relname NOT LIKE 'schema_migrations%'
		 ORDER BY c.relname
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
//
// It uses DELETE rather than TRUNCATE because Encore's per-service database role has
// CRUD privileges but not table ownership, and TRUNCATE requires ownership.
//
// DELETE respects foreign keys, so the tables are emptied child-before-parent using
// an order derived from the live constraint graph. Deriving it rather than hardcoding
// a list means this keeps working as later migrations add tables.
func TruncateAll(ctx context.Context) error {
	order, err := deletionOrder(ctx)
	if err != nil {
		return err
	}

	tx, err := DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, name := range order {
		// Names come from the system catalog, never from user input, so the quoted
		// identifier cannot be an injection vector.
		if _, err := tx.Exec(ctx, `DELETE FROM "`+name+`"`); err != nil {
			return fmt.Errorf("delete from %s: %w", name, err)
		}
	}
	return tx.Commit()
}

// deletionOrder returns application tables ordered so that every table appears before
// the tables it references. Deleting in this order never violates a foreign key.
func deletionOrder(ctx context.Context) ([]string, error) {
	names, err := tableNames(ctx)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil
	}

	isApp := make(map[string]bool, len(names))
	for _, n := range names {
		isApp[n] = true
	}

	// children[parent] = tables that reference parent. Those must be emptied first.
	children := make(map[string]map[string]bool, len(names))
	for _, n := range names {
		children[n] = map[string]bool{}
	}

	rows, err := DB.Query(ctx, `
		SELECT DISTINCT child.relname, parent.relname
		  FROM pg_constraint c
		  JOIN pg_class child  ON child.oid  = c.conrelid
		  JOIN pg_class parent ON parent.oid = c.confrelid
		  JOIN pg_namespace n   ON n.oid = child.relnamespace
		 WHERE c.contype = 'f'
		   AND n.nspname = 'public'
		   AND NOT child.relispartition
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		// A self-reference imposes no ordering between distinct tables.
		if child == parent || !isApp[child] || !isApp[parent] {
			continue
		}
		children[parent][child] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Kahn's algorithm: a table is safe to empty once nothing still-pending
	// references it, so referencing tables are always emptied first.
	order := make([]string, 0, len(names))
	done := make(map[string]bool, len(names))

	for len(order) < len(names) {
		progressed := false
		for _, n := range names {
			if done[n] {
				continue
			}
			blocked := false
			for child := range children[n] {
				if !done[child] {
					blocked = true
					break
				}
			}
			if !blocked {
				order = append(order, n)
				done[n] = true
				progressed = true
			}
		}
		if !progressed {
			// A foreign-key cycle. Append the remainder and let the caller's
			// transaction surface any genuine violation rather than looping forever.
			for _, n := range names {
				if !done[n] {
					order = append(order, n)
					done[n] = true
				}
			}
		}
	}
	return order, nil
}
