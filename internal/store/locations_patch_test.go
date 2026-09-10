package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// UpdateLocation exists so the HTTP PATCH in
// docs/specs/06-vision-shelf-ingestion.md — which may rename and re-parent in
// one call — lands as one transaction rather than as a rename followed by a
// move. These tests cover the part of that which is easy to get wrong: telling
// "leave this field alone" apart from "set it to null".

func TestUpdateLocationLeavesUnmentionedFieldsAlone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	description := "Behind the boiler"
	parent, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Basement"})
	require.NoError(t, err)
	node, err := s.CreateLocation(ctx, storageID, store.NewLocation{
		Name: "Right Shelf", Description: &description, ParentID: &parent.ID,
	})
	require.NoError(t, err)

	renamed := "Left Shelf"
	updated, err := s.UpdateLocation(ctx, storageID, node.ID, store.LocationPatch{Name: &renamed})
	require.NoError(t, err)

	assert.Equal(t, "Left Shelf", updated.Name)
	require.NotNil(t, updated.Description, "a rename must not erase a description nobody mentioned")
	assert.Equal(t, description, *updated.Description)
	require.NotNil(t, updated.ParentID, "nor detach the node from its parent")
	assert.Equal(t, parent.ID, *updated.ParentID)
}

func TestUpdateLocationClearsFieldsOnlyWhenAsked(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	description := "Behind the boiler"
	parent, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Basement"})
	require.NoError(t, err)
	node, err := s.CreateLocation(ctx, storageID, store.NewLocation{
		Name: "Right Shelf", Description: &description, ParentID: &parent.ID,
	})
	require.NoError(t, err)

	t.Run("an explicit null description clears it", func(t *testing.T) {
		updated, err := s.UpdateLocation(ctx, storageID, node.ID, store.LocationPatch{
			SetDescription: true, Description: nil,
		})
		require.NoError(t, err)
		assert.Nil(t, updated.Description)
	})

	t.Run("an explicit null parent promotes the node to a root", func(t *testing.T) {
		updated, err := s.UpdateLocation(ctx, storageID, node.ID, store.LocationPatch{
			SetParentID: true, ParentID: nil,
		})
		require.NoError(t, err)
		assert.Nil(t, updated.ParentID, "there is no other way to move a node to the top of the tree")
	})
}

// TestUpdateLocationAppliesRenameAndMoveTogether is the case that motivated a
// single store method: two store calls would be two transactions, and a failure
// between them would leave the node renamed but not moved.
func TestUpdateLocationAppliesRenameAndMoveTogether(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	oldParent, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Basement"})
	require.NoError(t, err)
	newParent, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Garage"})
	require.NoError(t, err)
	node, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shelf", ParentID: &oldParent.ID})
	require.NoError(t, err)

	renamed := "Tool Shelf"
	updated, err := s.UpdateLocation(ctx, storageID, node.ID, store.LocationPatch{
		Name: &renamed, ParentID: &newParent.ID, SetParentID: true,
	})
	require.NoError(t, err)

	assert.Equal(t, "Tool Shelf", updated.Name)
	require.NotNil(t, updated.ParentID)
	assert.Equal(t, newParent.ID, *updated.ParentID)

	// And it is the stored row that changed, not just the returned struct.
	var storedName string
	var storedParent *string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT name, parent_id::text FROM locations WHERE id = $1`, node.ID).Scan(&storedName, &storedParent))
	assert.Equal(t, "Tool Shelf", storedName)
	require.NotNil(t, storedParent)
	assert.Equal(t, newParent.ID.String(), *storedParent)
}

// TestUpdateLocationTouchesUpdatedAt keeps the client-delta contract in
// docs/specs/12-client-api-contract.md working: a change a client cannot see in
// updated_at is a change it will never sync.
func TestUpdateLocationTouchesUpdatedAt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	node, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shed"})
	require.NoError(t, err)

	renamed := "Garden Shed"
	updated, err := s.UpdateLocation(ctx, storageID, node.ID, store.LocationPatch{Name: &renamed})
	require.NoError(t, err)

	assert.True(t, updated.UpdatedAt.After(node.UpdatedAt),
		"updated_at must move forward or a cached client never learns about the rename")
}
