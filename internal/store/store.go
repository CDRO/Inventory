// Package store owns every database access in the system: the connection
// lifecycle, and one file per table group
// (docs/specs/04-backend-api-conventions.md).
//
// It is also where the data model's invariants live, because several of them
// cannot be stated in SQL (docs/specs/02-data-model.md):
//
//   - Same-storage validation on every id a caller supplies. A foreign key
//     proves the referenced row exists; it says nothing about which storage
//     owns it, so parent_id, category_id, location_id and every move target
//     are re-checked against the storage in the request.
//   - Cycle prevention on the locations and categories trees. A CHECK stops a
//     row being its own parent; a ring of two or more is invisible to the
//     schema, and a ring has no root, so it vanishes from every tree query
//     while its rows still exist and still hold inventory.
//   - The paired ledger write: every change to inventory_batches.quantity is
//     accompanied, in the same transaction, by an inventory_logs row
//     explaining it. writeLog is unexported and takes a transaction so that no
//     caller can move stock without also explaining it.
//   - Insert-only catalog_products with a one-level variant graph, and
//     tombstones written in the same transaction as the delete they record.
//
// Errors are the domain values in errors.go — ErrNotFound, ErrConflict,
// ErrValidation. Nothing in this package knows about HTTP; mapping them to
// status codes belongs to the handlers.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// undefinedTable is PostgreSQL's SQLSTATE for "relation does not exist".
const undefinedTable = "42P01"

// Store holds the connection pool. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// Open parses dsn and establishes the pool. It does not block on the database
// being reachable — readiness is reported by Ping, so the process can start and
// serve /healthz while PostgreSQL is still coming up.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool. It is safe to call on a nil *Store.
func (s *Store) Close() {
	if s == nil || s.pool == nil {
		return
	}
	s.pool.Close()
}

// Ping reports whether the database is currently reachable.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// Setting returns the value stored under key.
//
// The second result is false when no row exists — and also when the settings
// table has not been created yet, which is the normal state until the schema
// migration in docs/specs/02-data-model.md lands. Callers treat both as "no
// override", so a fresh deployment falls back to the environment instead of
// failing to start.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&value)
	switch {
	case err == nil:
		return value, true, nil
	case isNoRows(err), isUndefinedTable(err):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("store: read setting %q: %w", key, err)
	}
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == undefinedTable
}
