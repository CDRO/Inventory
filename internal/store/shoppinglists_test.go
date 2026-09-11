package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

func lines(texts ...string) []store.NewShoppingListItem {
	out := make([]store.NewShoppingListItem, 0, len(texts))
	for _, text := range texts {
		out = append(out, store.NewShoppingListItem{RawText: text, Status: store.ItemNewItem})
	}
	return out
}

// TestShoppingListKeepsEveryLineIndependently is the spec's first acceptance
// criterion: "A shopping list with N lines produces exactly N
// shopping_list_items rows, each independently resolvable (resolving one item
// doesn't block or auto-resolve others)."
func TestShoppingListKeepsEveryLineIndependently(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	_, items, err := s.CreateShoppingList(ctx, storageID, store.SourceText, nil,
		lines("milk", "eggs", "bread", "milk"))
	require.NoError(t, err)

	require.Len(t, items, 4, "four lines, four rows — including the repeated one")
	assert.Equal(t, 4, countRows(t, ctx,
		`SELECT count(*) FROM shopping_list_items WHERE shopping_list_id = $1`, items[0].ShoppingListID))

	// Resolve exactly one of them.
	resolved, err := s.ResolveShoppingListItem(ctx, storageID, items[1].ID, nil, 2)
	require.NoError(t, err)
	assert.Equal(t, store.ItemResolved, resolved.Status)
	require.NotNil(t, resolved.ResolvedQuantity)
	assert.Equal(t, 2, *resolved.ResolvedQuantity)

	// Every other line must be untouched — same status, still unresolved.
	assert.Equal(t, 3, countRows(t, ctx,
		`SELECT count(*) FROM shopping_list_items WHERE shopping_list_id = $1 AND status <> 'resolved'`,
		items[0].ShoppingListID))

	for _, other := range []store.ShoppingListItem{items[0], items[2], items[3]} {
		again, err := s.ShoppingListItemByID(ctx, storageID, other.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ItemNewItem, again.Status, "resolving a sibling must not resolve this line")
		assert.Nil(t, again.ResolvedQuantity)
	}
}

// TestResolvingTwiceIsRefused — the second resolve would double whatever
// inventory the first one created.
func TestResolvingTwiceIsRefused(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	_, items, err := s.CreateShoppingList(ctx, storageID, store.SourceText, nil, lines("milk"))
	require.NoError(t, err)

	_, err = s.ResolveShoppingListItem(ctx, storageID, items[0].ID, nil, 1)
	require.NoError(t, err)

	_, err = s.ResolveShoppingListItem(ctx, storageID, items[0].ID, nil, 1)
	assert.ErrorIs(t, err, store.ErrConflict)
}

// TestRematchIsRefusedOnceResolved — re-matching a resolved line would discard
// a decision the user already made, and possibly one that already wrote stock.
func TestRematchIsRefusedOnceResolved(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	_, items, err := s.CreateShoppingList(ctx, storageID, store.SourceText, nil, lines("mlik"))
	require.NoError(t, err)

	// Correcting the typo before resolving is fine.
	fixed, err := s.RematchShoppingListItem(ctx, storageID, items[0].ID, "milk", store.ItemNewItem, nil)
	require.NoError(t, err)
	assert.Equal(t, "milk", fixed.RawText)

	_, err = s.ResolveShoppingListItem(ctx, storageID, items[0].ID, nil, 1)
	require.NoError(t, err)

	_, err = s.RematchShoppingListItem(ctx, storageID, items[0].ID, "oat milk", store.ItemNewItem, nil)
	assert.ErrorIs(t, err, store.ErrConflict)
}

// TestShoppingListsAreStorageScoped — a list id from another storage must be
// indistinguishable from one that never existed, at every entry point.
func TestShoppingListsAreStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	_, items, err := s.CreateShoppingList(ctx, storageB, store.SourceText, nil, lines("their milk"))
	require.NoError(t, err)
	theirList := items[0].ShoppingListID
	theirItem := items[0].ID

	t.Run("reading a list", func(t *testing.T) {
		_, _, err := s.ShoppingListWithItems(ctx, storageA, theirList)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("reading an item", func(t *testing.T) {
		_, err := s.ShoppingListItemByID(ctx, storageA, theirItem)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("resolving an item", func(t *testing.T) {
		_, err := s.ResolveShoppingListItem(ctx, storageA, theirItem, nil, 1)
		assert.ErrorIs(t, err, store.ErrNotFound)

		still, err := s.ShoppingListItemByID(ctx, storageB, theirItem)
		require.NoError(t, err)
		assert.NotEqual(t, store.ItemResolved, still.Status, "the refusal must also not have written anything")
	})

	t.Run("re-matching an item", func(t *testing.T) {
		_, err := s.RematchShoppingListItem(ctx, storageA, theirItem, "mine now", store.ItemNewItem, nil)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("a nonexistent id is the same error", func(t *testing.T) {
		_, foreignErr := s.ShoppingListItemByID(ctx, storageA, theirItem)
		_, missingErr := s.ShoppingListItemByID(ctx, storageA, uuid.New())

		assert.Equal(t, foreignErr.Error(), missingErr.Error())
	})
}

// TestAMatchedProductMustBelongToThisStorage closes the other direction: the
// line is ours, but the product it claims to match is not.
func TestAMatchedProductMustBelongToThisStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	foreign, err := s.CreateProduct(ctx, storageB, store.NewProduct{Name: "Their Milk"})
	require.NoError(t, err)

	t.Run("at creation", func(t *testing.T) {
		_, _, err := s.CreateShoppingList(ctx, storageA, store.SourceText, nil,
			[]store.NewShoppingListItem{{
				RawText: "milk", Status: store.ItemExactMatch, MatchedProductID: &foreign.ID,
			}})
		assert.ErrorIs(t, err, store.ErrNotFound)

		assert.Zero(t, countRows(t, ctx,
			`SELECT count(*) FROM shopping_lists WHERE storage_id = $1`, storageA),
			"the whole list is rolled back, not left half-written")
	})

	t.Run("at resolution", func(t *testing.T) {
		_, items, err := s.CreateShoppingList(ctx, storageA, store.SourceText, nil, lines("milk"))
		require.NoError(t, err)

		_, err = s.ResolveShoppingListItem(ctx, storageA, items[0].ID, &foreign.ID, 1)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestAnEmptyShoppingListIsRejected(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	_, _, err := s.CreateShoppingList(ctx, newStorage(t, ctx), store.SourceText, nil, nil)

	assert.ErrorIs(t, err, store.ErrValidation)
}
