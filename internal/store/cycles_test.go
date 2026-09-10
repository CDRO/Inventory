package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// Cycle prevention is the other invariant no CHECK can express. The
// locations_not_own_parent constraint stops the one-node case; a ring of two or
// more is invisible to the schema.
//
// The consequence is worse than an odd-looking tree: a ring has no root, so it
// disappears from every tree query while its rows still exist and still hold
// inventory. The stock is neither visible nor deletable through the UI.

// chain builds root → child → grandchild and returns their ids.
func locationChain(t *testing.T, ctx context.Context, s *store.Store, storageID uuid.UUID) (root, child, grandchild uuid.UUID) {
	t.Helper()

	r, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Basement"})
	require.NoError(t, err)
	c, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Right Shelf", ParentID: &r.ID})
	require.NoError(t, err)
	g, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Layer 2", ParentID: &c.ID})
	require.NoError(t, err)
	return r.ID, c.ID, g.ID
}

func TestMoveLocationRejectsCycles(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	root, child, grandchild := locationChain(t, ctx, s, storageID)

	tests := []struct {
		name   string
		node   uuid.UUID
		parent uuid.UUID
	}{
		{name: "node under itself", node: root, parent: root},
		{name: "root under its direct child", node: root, parent: child},
		{name: "root under a deeper descendant", node: root, parent: grandchild},
		{name: "child under its own child", node: child, parent: grandchild},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.MoveLocation(ctx, storageID, tc.node, &tc.parent)
			require.ErrorIs(t, err, store.ErrConflict)
		})
	}

	// The tree must be exactly as it started: every node still reachable from
	// the root.
	var rootParent, childParent, grandchildParent *uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx, `SELECT parent_id FROM locations WHERE id = $1`, root).Scan(&rootParent))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT parent_id FROM locations WHERE id = $1`, child).Scan(&childParent))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT parent_id FROM locations WHERE id = $1`, grandchild).Scan(&grandchildParent))

	assert.Nil(t, rootParent)
	require.NotNil(t, childParent)
	assert.Equal(t, root, *childParent)
	require.NotNil(t, grandchildParent)
	assert.Equal(t, child, *grandchildParent)
}

func TestMoveCategoryRejectsCycles(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	food, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	dairy, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy", ParentID: &food.ID})
	require.NoError(t, err)
	cheese, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Cheese", ParentID: &dairy.ID})
	require.NoError(t, err)

	assert.ErrorIs(t, s.MoveCategory(ctx, storageID, food.ID, &cheese.ID), store.ErrConflict)
	assert.ErrorIs(t, s.MoveCategory(ctx, storageID, food.ID, &food.ID), store.ErrConflict)
	assert.ErrorIs(t, s.MoveCategory(ctx, storageID, dairy.ID, &cheese.ID), store.ErrConflict)
}

// TestLegitimateMovesStillWork guards against the cycle check being so eager
// that it blocks ordinary re-parenting. A test suite that only proves the
// rejections would pass with a function that rejects everything.
func TestLegitimateMovesStillWork(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	root, child, grandchild := locationChain(t, ctx, s, storageID)

	t.Run("promote a grandchild to a root", func(t *testing.T) {
		require.NoError(t, s.MoveLocation(ctx, storageID, grandchild, nil))

		var parent *uuid.UUID
		require.NoError(t, testPool.QueryRow(ctx,
			`SELECT parent_id FROM locations WHERE id = $1`, grandchild).Scan(&parent))
		assert.Nil(t, parent)
	})

	t.Run("move a former descendant back under the root", func(t *testing.T) {
		require.NoError(t, s.MoveLocation(ctx, storageID, grandchild, &root))

		var parent *uuid.UUID
		require.NoError(t, testPool.QueryRow(ctx,
			`SELECT parent_id FROM locations WHERE id = $1`, grandchild).Scan(&parent))
		require.NotNil(t, parent)
		assert.Equal(t, root, *parent)
	})

	t.Run("move a sibling under another sibling", func(t *testing.T) {
		require.NoError(t, s.MoveLocation(ctx, storageID, grandchild, &child))
	})
}

// TestMoveTouchesUpdatedAt covers the delta-sync column: a re-parent that did
// not bump updated_at would be invisible to a client asking for "everything
// changed since X", leaving the node under its old parent forever.
func TestMoveTouchesUpdatedAt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	root, child, _ := locationChain(t, ctx, s, storageID)

	var before, after any
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT updated_at FROM locations WHERE id = $1`, child).Scan(&before))

	require.NoError(t, s.MoveLocation(ctx, storageID, child, nil))

	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT updated_at FROM locations WHERE id = $1`, child).Scan(&after))

	assert.NotEqual(t, before, after, "a move must bump updated_at for delta sync")

	// Restore so the fixture reads naturally if this test is extended.
	require.NoError(t, s.MoveLocation(ctx, storageID, child, &root))
}

// TestRenameTouchesUpdatedAt covers the same column on the other write path.
func TestRenameTouchesUpdatedAt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	loc, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shed"})
	require.NoError(t, err)

	var before, after any
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT updated_at FROM locations WHERE id = $1`, loc.ID).Scan(&before))

	require.NoError(t, s.RenameLocation(ctx, storageID, loc.ID, "Garden Shed", nil))

	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT updated_at FROM locations WHERE id = $1`, loc.ID).Scan(&after))
	assert.NotEqual(t, before, after)
}

// TestRenameRejectsForeignRow keeps the same-storage rule on the rename path,
// which does its check in the WHERE clause rather than via requireSameStorage.
func TestRenameRejectsForeignRow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	theirs, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Theirs"})
	require.NoError(t, err)

	err = s.RenameLocation(ctx, storageA, theirs.ID, "Mine Now", nil)

	require.ErrorIs(t, err, store.ErrNotFound)
	var name string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT name FROM locations WHERE id = $1`, theirs.ID).Scan(&name))
	assert.Equal(t, "Theirs", name)
}

// TestUpdateLocationRejectsCycles keeps the cycle guard on the combined patch
// path. UpdateLocation runs its own check rather than delegating to
// MoveLocation — it has to, since it applies the rename in the same
// transaction — so the guard that already covers MoveLocation proves nothing
// here.
func TestUpdateLocationRejectsCycles(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	root, child, grandchild := locationChain(t, ctx, s, storageID)

	tests := []struct {
		name   string
		node   uuid.UUID
		parent uuid.UUID
	}{
		{"under itself", root, root},
		{"under its own child", root, child},
		{"under its own grandchild", root, grandchild},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.UpdateLocation(ctx, storageID, tc.node, store.LocationPatch{
				ParentID: &tc.parent, SetParentID: true,
			})

			require.ErrorIs(t, err, store.ErrConflict)
		})
	}

	// The rename in the same patch must not have landed either: a rejected
	// move that still renamed the node would be the half-applied write this
	// single transaction exists to prevent.
	name := "Basement"
	_, err := s.UpdateLocation(ctx, storageID, root, store.LocationPatch{
		Name: &name, ParentID: &child, SetParentID: true,
	})
	require.ErrorIs(t, err, store.ErrConflict)

	var stored string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT name FROM locations WHERE id = $1`, root).Scan(&stored))
	assert.Equal(t, "Basement", stored, "the fixture's original name, unchanged by the rejected patch")
}
