package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// TestExportStorageCarriesNothingFromAnotherStorage is the acceptance
// criterion of docs/specs/15-backup-restore-and-export.md that matters most,
// checked the only way that proves anything: against a real database holding a
// second storage full of deliberately confusable data.
//
// Both storages get products with the same names, locations with the same
// names, a shared catalog row, batches, ledger rows and a shopping list — and
// one user who is a member of both, since a fixture where the two households
// share nothing would pass even if the queries had no WHERE clause at all.
//
// The assertion is made against the serialized bytes rather than against the
// struct fields, because that is the thing that ships. A field nobody thought
// to check is still in the JSON.
func TestExportStorageCarriesNothingFromAnotherStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	mine := newStorage(t, ctx)
	theirs := newStorage(t, ctx)
	shared := newUser(t, ctx)

	// One catalog row both storages' products point at: global, anonymous and
	// cross-household by design (docs/specs/02-data-model.md), so it is the
	// most plausible route for one storage's id to reach the other's archive.
	catalog, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Butter " + mine.String()[:8],
		ItemType:    store.ItemPerishable,
	})
	require.NoError(t, err)

	seedStorage := func(storageID uuid.UUID, label string) (uuid.UUID, uuid.UUID, uuid.UUID) {
		location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
		require.NoError(t, err)
		category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy"})
		require.NoError(t, err)
		product, err := s.CreateProduct(ctx, storageID, store.NewProduct{
			Name: "Butter", CategoryID: &category.ID, CatalogID: &catalog.ID,
			ItemType: store.ItemPerishable, MinStock: 2,
		})
		require.NoError(t, err)
		_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
			ProductID: product.ID, LocationID: location.ID, Quantity: 4,
			Reason: store.ReasonPurchase, CreatedBy: &shared,
		})
		require.NoError(t, err)
		_, _, err = s.CreateShoppingList(ctx, storageID, store.SourceText, &shared,
			[]store.NewShoppingListItem{{RawText: label, Status: store.ItemNewItem}})
		require.NoError(t, err)
		return location.ID, category.ID, product.ID
	}

	seedStorage(mine, "mine")
	otherLocation, otherCategory, otherProduct := seedStorage(theirs, "theirs")

	export, err := s.ExportStorage(ctx, mine)
	require.NoError(t, err)

	raw, err := json.Marshal(export)
	require.NoError(t, err)
	body := string(raw)

	for name, id := range map[string]uuid.UUID{
		"the other storage":     theirs,
		"its location":          otherLocation,
		"its category":          otherCategory,
		"its product":           otherProduct,
		"the shared catalog id": catalog.ID,
	} {
		assert.NotContains(t, body, id.String(), "the archive must carry no identifier of %s", name)
	}

	// And the positive half, so the test cannot pass by exporting nothing.
	assert.Equal(t, mine, export.Storage.ID)
	require.Len(t, export.Products, 1)
	require.Len(t, export.Locations, 1)
	require.Len(t, export.Categories, 1)
	require.Len(t, export.Batches, 1)
	require.Len(t, export.ShoppingLists, 1)
	require.Len(t, export.ShoppingLists[0].Items, 1)
	assert.Equal(t, "mine", export.ShoppingLists[0].Items[0].RawText)
}

// TestExportStorageCarriesNoCredentialMaterial is the other half of the "one
// artifact designed to leave the house" criterion: an export is a file a
// member can mail to themselves, so nothing that authenticates anyone may be
// in it.
//
// Scanned against the serialized bytes for the same reason as above.
func TestExportStorageCarriesNoCredentialMaterial(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	const hash = "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$do-not-export-me"
	admin, err := s.CreateUser(ctx, store.NewUser{
		Username:     "admin-" + storageID.String()[:8],
		PasswordHash: hash,
		DisplayName:  "Alex Admin",
		IsAdmin:      true,
	})
	require.NoError(t, err)
	require.NoError(t, s.AddMember(ctx, storageID, admin.ID))

	session, err := s.CreateSession(ctx, admin.ID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)

	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1,
		Reason: store.ReasonPurchase, CreatedBy: &admin.ID,
	})
	require.NoError(t, err)

	export, err := s.ExportStorage(ctx, storageID)
	require.NoError(t, err)
	raw, err := json.Marshal(export)
	require.NoError(t, err)
	body := string(raw)

	assert.NotContains(t, body, hash, "a password hash must never reach an archive")
	assert.NotContains(t, body, session.ID, "a session id is a bearer credential")
	assert.NotContains(t, body, admin.ID.String(), "a user id is resolved to a display name, never carried")
	assert.NotContains(t, strings.ToLower(body), "is_admin", "admin status is not this storage's data")

	// The display name is what replaces the id — the part that still means
	// something when the archive is opened elsewhere.
	require.Len(t, export.Logs, 1)
	require.NotNil(t, export.Logs[0].CreatedBy)
	assert.Equal(t, "Alex Admin", *export.Logs[0].CreatedBy)
}

// TestExportStorageHandlesALedgerRowWhoseUserIsGone — inventory_logs.created_by
// is ON DELETE SET NULL, so "somebody who no longer has an account did this"
// is an ordinary state of the table. The export has to render it as a null
// rather than dropping the row: a ledger with holes in it is not a ledger.
func TestExportStorageHandlesALedgerRowWhoseUserIsGone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	gone := newUser(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1,
		Reason: store.ReasonPurchase, CreatedBy: &gone,
	})
	require.NoError(t, err)
	require.NoError(t, s.DeleteUser(ctx, gone))

	export, err := s.ExportStorage(ctx, storageID)
	require.NoError(t, err)

	require.Len(t, export.Logs, 1, "the ledger row outlives the account that wrote it")
	assert.Nil(t, export.Logs[0].CreatedBy)
	assert.Equal(t, 1, export.Logs[0].ChangeQty)
}

// TestExportStorageReportsCurrentStockPerProduct — current_stock is the live
// sum of a product's batches, computed in the export query rather than by the
// caller, and it is zero for a product with no batches at all rather than
// absent.
func TestExportStorageReportsCurrentStockPerProduct(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	stocked, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk"})
	require.NoError(t, err)
	_, err = s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Salt"})
	require.NoError(t, err)

	for _, qty := range []int{3, 4} {
		_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
			ProductID: stocked.ID, LocationID: location.ID, Quantity: qty,
			Reason: store.ReasonPurchase,
		})
		require.NoError(t, err)
	}

	export, err := s.ExportStorage(ctx, storageID)
	require.NoError(t, err)

	stock := map[string]int{}
	for _, p := range export.Products {
		stock[p.Name] = p.CurrentStock
	}
	assert.Equal(t, 7, stock["Milk"], "stock is summed across every batch of the product")
	assert.Equal(t, 0, stock["Salt"], "a product with no batches exports as zero, not as missing")
}

// TestExportStorageIsNotFoundForAnUnknownStorage — the same ErrNotFound every
// other storage-scoped read answers, which is what the handler turns into the
// standard 404 (docs/specs/03-auth-and-multi-tenancy.md).
func TestExportStorageIsNotFoundForAnUnknownStorage(t *testing.T) {
	s := requireDB(t)

	_, err := s.ExportStorage(context.Background(), uuid.New())
	assert.ErrorIs(t, err, store.ErrNotFound)
}
