package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// treeTable names one of the two self-referencing, storage-scoped trees.
//
// It is a closed type with unexported constants rather than a plain string so
// that a table name can never arrive from a caller and reach SQL. The values
// below are the only ones that exist.
type treeTable string

const (
	treeLocations  treeTable = "locations"
	treeCategories treeTable = "categories"
)

// maxTreeDepth bounds the ancestor walk. A correct tree is a handful of levels
// deep (`Basement → Right Shelf → Layer 2 → Front-Right`); the bound exists so
// that already-corrupt data cannot turn a cycle check into an unbounded query.
const maxTreeDepth = 64

// requireSameStorage confirms that id names a row of this tree belonging to
// storageID.
//
// This is the invariant PostgreSQL cannot express: a composite foreign key
// would pin parent_id to the same storage, but the same rule also applies to
// products.category_id, inventory_batches.location_id and every id arriving in
// a request body, so it lives here where every write path shares it.
//
// A row in another storage returns ErrNotFound, the same error a nonexistent
// id returns — see the comment on ErrNotFound.
func requireSameStorage(ctx context.Context, q querier, table treeTable, storageID, id uuid.UUID) error {
	var found bool
	// #nosec G201 -- table is a treeTable constant, never caller-supplied.
	sql := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE id = $1 AND storage_id = $2)`, table)

	if err := q.QueryRow(ctx, sql, id, storageID).Scan(&found); err != nil {
		return fmt.Errorf("store: check %s %s in storage: %w", table, id, err)
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

// wouldCycle reports whether re-parenting node under parent would create a
// cycle — that is, whether node is parent itself or one of parent's ancestors.
//
// Without this a user can detach a subtree from the tree entirely: point a
// node at its own descendant and the resulting ring is unreachable from any
// root, so it vanishes from every tree query while its rows still exist and
// still hold inventory.
func wouldCycle(ctx context.Context, q querier, table treeTable, node, parent uuid.UUID) (bool, error) {
	if node == parent {
		return true, nil
	}

	// Walk from the proposed parent up towards the root. If node appears on
	// that path, parent already sits underneath it.
	// #nosec G201 -- table is a treeTable constant, never caller-supplied.
	sql := fmt.Sprintf(`
		WITH RECURSIVE ancestors AS (
		    SELECT id, parent_id, 1 AS depth
		      FROM %[1]s
		     WHERE id = $1
		    UNION ALL
		    SELECT t.id, t.parent_id, a.depth + 1
		      FROM %[1]s t
		      JOIN ancestors a ON t.id = a.parent_id
		     WHERE a.depth < %[2]d
		)
		SELECT EXISTS (SELECT 1 FROM ancestors WHERE id = $2)`, table, maxTreeDepth)

	var cycles bool
	if err := q.QueryRow(ctx, sql, parent, node).Scan(&cycles); err != nil {
		return false, fmt.Errorf("store: cycle check on %s: %w", table, err)
	}
	return cycles, nil
}

// resolveParent validates a proposed parent for a node in this storage.
//
// A nil parent is a root node and always valid. Otherwise the parent must live
// in the same storage, and — when node is non-nil, i.e. this is a move rather
// than an insert — must not sit beneath node.
func resolveParent(ctx context.Context, q querier, table treeTable, storageID uuid.UUID, node *uuid.UUID, parent *uuid.UUID) error {
	if parent == nil {
		return nil
	}
	if err := requireSameStorage(ctx, q, table, storageID, *parent); err != nil {
		return err
	}
	if node == nil {
		return nil
	}
	cycles, err := wouldCycle(ctx, q, table, *node, *parent)
	if err != nil {
		return err
	}
	if cycles {
		return fmt.Errorf("%w: re-parenting would create a cycle in %s", ErrConflict, table)
	}
	return nil
}

// querier is the surface shared by *pgxpool.Pool and pgx.Tx, so every helper
// here works both inside and outside a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// scanCount reads a single-column count query, treating "no rows" as zero.
func scanCount(ctx context.Context, q querier, sql string, args ...any) (int, error) {
	var n int
	if err := q.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}
