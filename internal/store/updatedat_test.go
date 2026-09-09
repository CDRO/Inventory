package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// updated_at is what a client's delta sync asks against: "everything changed
// since X". A write path that forgets to bump it is invisible to that query,
// so the client keeps serving the stale row from its cache indefinitely —
// with no error anywhere to notice.
//
// Every write path therefore needs its own assertion. Testing one path proves
// nothing about the others, because each carries the `updated_at = now()`
// itself rather than inheriting it from a trigger (the spec's choice, so the
// mechanism is visible where the write happens).

func readUpdatedAt(t *testing.T, ctx context.Context, table string, id uuid.UUID) time.Time {
	t.Helper()

	var at time.Time
	switch table {
	case "products":
		require.NoError(t, testPool.QueryRow(ctx,
			`SELECT updated_at FROM products WHERE id = $1`, id).Scan(&at))
	case "categories":
		require.NoError(t, testPool.QueryRow(ctx,
			`SELECT updated_at FROM categories WHERE id = $1`, id).Scan(&at))
	case "locations":
		require.NoError(t, testPool.QueryRow(ctx,
			`SELECT updated_at FROM locations WHERE id = $1`, id).Scan(&at))
	default:
		t.Fatalf("unknown table %q", table)
	}
	return at
}

// age backdates updated_at so a bump is unmistakable even when the write lands
// inside the same clock tick.
func age(t *testing.T, ctx context.Context, table string, id uuid.UUID) time.Time {
	t.Helper()

	switch table {
	case "products":
		_, err := execTest(ctx, `UPDATE products SET updated_at = now() - interval '1 hour' WHERE id = $1`, id)
		require.NoError(t, err)
	case "categories":
		_, err := execTest(ctx, `UPDATE categories SET updated_at = now() - interval '1 hour' WHERE id = $1`, id)
		require.NoError(t, err)
	case "locations":
		_, err := execTest(ctx, `UPDATE locations SET updated_at = now() - interval '1 hour' WHERE id = $1`, id)
		require.NoError(t, err)
	}
	return readUpdatedAt(t, ctx, table, id)
}

func TestSetProductCategoryTouchesUpdatedAt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Yoghurt"})
	require.NoError(t, err)
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy"})
	require.NoError(t, err)

	before := age(t, ctx, "products", product.ID)

	require.NoError(t, s.SetProductCategory(ctx, storageID, product.ID, &category.ID))

	after := readUpdatedAt(t, ctx, "products", product.ID)
	assert.True(t, after.After(before),
		"re-categorising a product must bump updated_at or a client keeps the old category forever")
}

func TestSetProductCategoryToNullTouchesUpdatedAt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Yoghurt", CategoryID: &category.ID})
	require.NoError(t, err)

	before := age(t, ctx, "products", product.ID)

	// Clearing a category is a change like any other.
	require.NoError(t, s.SetProductCategory(ctx, storageID, product.ID, nil))

	after := readUpdatedAt(t, ctx, "products", product.ID)
	assert.True(t, after.After(before))
}

func TestMoveCategoryTouchesUpdatedAt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	food, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	dairy, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy"})
	require.NoError(t, err)

	before := age(t, ctx, "categories", dairy.ID)

	require.NoError(t, s.MoveCategory(ctx, storageID, dairy.ID, &food.ID))

	after := readUpdatedAt(t, ctx, "categories", dairy.ID)
	assert.True(t, after.After(before))
}

func TestSetCategoryShelfLifeTouchesUpdatedAt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy"})
	require.NoError(t, err)

	before := age(t, ctx, "categories", category.ID)

	require.NoError(t, s.SetCategoryShelfLife(ctx, storageID, category.ID, ptrInt(10)))

	after := readUpdatedAt(t, ctx, "categories", category.ID)
	assert.True(t, after.After(before),
		"a shelf-life change is exactly the kind of edit a client must pick up")
}

func TestSetCategoryShelfLifeRejectsForeignRow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	theirs, err := s.CreateCategory(ctx, storageB, store.NewCategory{Name: "Theirs"})
	require.NoError(t, err)

	err = s.SetCategoryShelfLife(ctx, storageA, theirs.ID, ptrInt(1))

	require.ErrorIs(t, err, store.ErrNotFound)
	var days *int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT default_shelf_life_days FROM categories WHERE id = $1`, theirs.ID).Scan(&days))
	assert.Nil(t, days, "another storage must not be able to change this rule")
}

// TestConcurrentOppositeMovesCannotFormARing is the regression for the race
// the Go review found in round 1.
//
// Two moves in opposite directions — A under B, and B under A — each read a
// tree in which the other has not yet committed. Without serialisation both
// cycle checks pass and the result is a two-node ring: no root, so the pair
// disappears from every tree query while its rows still exist. Exactly one of
// the two moves must therefore win.
func TestConcurrentOppositeMovesCannotFormARing(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	for attempt := 0; attempt < 10; attempt++ {
		storageID := newStorage(t, ctx)

		a, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "A"})
		require.NoError(t, err)
		b, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "B"})
		require.NoError(t, err)

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		start := make(chan struct{})

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errs <- s.MoveLocation(ctx, storageID, a.ID, &b.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			errs <- s.MoveLocation(ctx, storageID, b.ID, &a.ID)
		}()

		close(start)
		wg.Wait()
		close(errs)

		var wins int
		for err := range errs {
			if err == nil {
				wins++
			} else {
				require.ErrorIs(t, err, store.ErrConflict,
					"the losing move must be refused as a cycle, not fail some other way")
			}
		}
		require.Equalf(t, 1, wins, "attempt %d: exactly one of two opposite moves may succeed", attempt)

		// The decisive check: at least one node still has no parent, so the
		// pair is still reachable from a root rather than pointing at itself.
		roots := countRows(t, ctx,
			`SELECT count(*) FROM locations WHERE storage_id = $1 AND parent_id IS NULL`, storageID)
		require.Equalf(t, 1, roots, "attempt %d: the tree must keep exactly one root, not become a ring", attempt)
	}
}

// TestCreateBatchRejectsZeroQuantity — a batch is a quantity of something in a
// place. Allowing zero would create the row AdjustBatch deletes on sight, and
// pair it with a ledger entry recording that nothing happened.
func TestCreateBatchRejectsZeroQuantity(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
	require.NoError(t, err)

	for _, qty := range []int{0, -1} {
		_, err := s.CreateBatch(ctx, storageID, store.NewBatch{
			ProductID: product.ID, LocationID: location.ID, Quantity: qty, Reason: store.ReasonPurchase,
		})
		require.ErrorIsf(t, err, store.ErrValidation, "quantity %d must be refused", qty)
	}

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM inventory_batches WHERE product_id = $1`, product.ID))
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE product_id = $1`, product.ID),
		"no phantom batch means no ledger row explaining it")
}
