package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// fakeCategories is an in-memory CategoryStore that records what it was asked
// to do, the same way fakeLocations does.
type fakeCategories struct {
	tree          []store.Category
	treeErr       error
	lastStorageID uuid.UUID

	createErr  error
	lastCreate store.NewCategory
	lastUserID uuid.UUID

	updateErr error
	lastPatch store.CategoryPatch
	lastID    uuid.UUID
	updates   int

	deleteErr error
	deleted   []uuid.UUID

	delta      *store.Delta[store.Category]
	deltaErr   error
	lastSince  time.Time
	deltaCalls int
}

func (f *fakeCategories) CategoryTree(_ context.Context, storageID uuid.UUID) ([]store.Category, error) {
	f.lastStorageID = storageID
	return f.tree, f.treeErr
}

func (f *fakeCategories) CategoriesChangedSince(_ context.Context, storageID uuid.UUID, since time.Time) (*store.Delta[store.Category], error) {
	f.lastStorageID = storageID
	f.lastSince = since
	f.deltaCalls++
	if f.deltaErr != nil {
		return nil, f.deltaErr
	}
	if f.delta != nil {
		return f.delta, nil
	}
	return &store.Delta[store.Category]{Changed: f.tree, Deleted: []uuid.UUID{}, SyncedAt: fixedSyncPoint}, nil
}

func (f *fakeCategories) CreateCategoryAsUser(_ context.Context, storageID uuid.UUID, in store.NewCategory, userID uuid.UUID) (*store.Category, error) {
	f.lastStorageID = storageID
	f.lastCreate = in
	f.lastUserID = userID
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &store.Category{
		ID: uuid.New(), StorageID: storageID, ParentID: in.ParentID,
		Name: in.Name, DefaultShelfLifeDays: in.DefaultShelfLifeDays,
	}, nil
}

func (f *fakeCategories) UpdateCategory(_ context.Context, storageID, id uuid.UUID, patch store.CategoryPatch) (*store.Category, error) {
	f.lastStorageID = storageID
	f.lastID = id
	f.lastPatch = patch
	f.updates++
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	name := "Updated"
	if patch.Name != nil {
		name = *patch.Name
	}
	return &store.Category{ID: id, StorageID: storageID, ParentID: patch.ParentID, Name: name}, nil
}

func (f *fakeCategories) DeleteCategory(_ context.Context, storageID, id uuid.UUID) error {
	f.lastStorageID = storageID
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

// TestCategoryTreeNestsChildrenWhateverTheRowOrder is CategoryTree's
// counterpart to TestLocationTreeNestsChildrenWhateverTheRowOrder
// (locations_test.go): CategoryTree orders by created_at, which stops
// putting parents first the moment an old node is re-parented under a newer
// one, and nestCategories has to handle that the same way nestLocations does.
func TestCategoryTreeNestsChildrenWhateverTheRowOrder(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	parent := uuid.New()
	child := uuid.New()

	f.categories.tree = []store.Category{
		{ID: child, StorageID: f.storageID, ParentID: &parent, Name: "Dairy"},
		{ID: parent, StorageID: f.storageID, Name: "Food"},
	}

	rec := f.do(http.MethodGet, f.base()+"/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Items []struct {
			ID       uuid.UUID `json:"id"`
			Name     string    `json:"name"`
			Children []struct {
				ID   uuid.UUID `json:"id"`
				Name string    `json:"name"`
			} `json:"children"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Len(t, body.Items, 1, "the child must not surface as a second root")
	assert.Equal(t, parent, body.Items[0].ID)
	require.Len(t, body.Items[0].Children, 1)
	assert.Equal(t, child, body.Items[0].Children[0].ID)
}

// TestCategoryTreeResponseNeverCarriesStorageID asserts on the wire format,
// the same reason locationNode's counterpart test does: the field must not
// exist for a client to read, not merely go unused.
func TestCategoryTreeResponseNeverCarriesStorageID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.categories.tree = []store.Category{{ID: uuid.New(), StorageID: f.storageID, Name: "Pantry"}}

	rec := f.do(http.MethodGet, f.base()+"/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "storage_id")
}

// TestCategoryTreeIsStorageScoped confirms the handler reads storageID from
// the request context (the RequireStorageMember-verified value), not from
// anywhere a caller could supply directly.
func TestCategoryTreeIsStorageScoped(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.categories.tree = []store.Category{{ID: uuid.New(), StorageID: f.storageID, Name: "Pantry"}}

	rec := f.do(http.MethodGet, f.base()+"/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, f.storageID, f.categories.lastStorageID)
}

// TestCategoryTreeEmptyIsAnEmptyListNotNull matches every other collection
// endpoint in this package: "items": [] on nothing found, never null.
func TestCategoryTreeEmptyIsAnEmptyListNotNull(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, f.base()+"/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"items":[]`)
}

// TestCategoryTreeCarriesTheShelfLifeRule: the category page edits the rule
// per node, so each node must say what it is — and a node with no rule of its
// own must say null ("inherit") explicitly rather than omit the field, which a
// client could not tell apart from an older server that never sent it.
func TestCategoryTreeCarriesTheShelfLifeRule(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	food := uuid.New()
	days := 365
	f.categories.tree = []store.Category{
		{ID: food, StorageID: f.storageID, Name: "Food", DefaultShelfLifeDays: &days},
		{ID: uuid.New(), StorageID: f.storageID, ParentID: &food, Name: "Dairy"},
	}

	rec := f.do(http.MethodGet, f.base()+"/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Items []struct {
			DefaultShelfLifeDays *int                         `json:"default_shelf_life_days"`
			Children             []map[string]json.RawMessage `json:"children"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	require.NotNil(t, body.Items[0].DefaultShelfLifeDays)
	assert.Equal(t, 365, *body.Items[0].DefaultShelfLifeDays)

	require.Len(t, body.Items[0].Children, 1)
	raw, present := body.Items[0].Children[0]["default_shelf_life_days"]
	require.True(t, present, "an inheriting node still carries the field")
	assert.Equal(t, "null", string(raw))
}

func TestCreateCategoryValidatesItsInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		body  string
		field string
	}{
		{"empty name", `{"name":""}`, "name"},
		{"whitespace-only name", `{"name":"   "}`, "name"},
		{"name too long", `{"name":"` + strings.Repeat("x", 256) + `"}`, "name"},
		{"malformed parent", `{"name":"Food","parent_id":"not-a-uuid"}`, "parent_id"},
		{"negative shelf life", `{"name":"Food","default_shelf_life_days":-1}`, "default_shelf_life_days"},
		{"shelf life not a number", `{"name":"Food","default_shelf_life_days":"ten"}`, "default_shelf_life_days"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPost, f.base()+"/categories", tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Equal(t, "validation_failed", errorCode(t, rec))
			assert.NotEmpty(t, errorFields(t, rec)[tc.field], "the form needs to know which field to mark")
		})
	}

	t.Run("every bad field is reported at once", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		rec := f.do(http.MethodPost, f.base()+"/categories", `{"name":"","default_shelf_life_days":-1}`)

		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		fields := errorFields(t, rec)
		assert.NotEmpty(t, fields["name"])
		assert.NotEmpty(t, fields["default_shelf_life_days"])
	})
}

// TestCreateCategoryPassesItsInputThrough: the storage comes from the URL the
// middleware validated, never the body; the acting user is the session's, so
// the category_created contribution lands on the person who made it; and a
// shelf life given at creation reaches the store.
func TestCreateCategoryPassesItsInputThrough(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	parent := uuid.New()
	foreign := uuid.New()

	rec := f.do(http.MethodPost, f.base()+"/categories",
		`{"name":"  Cheese ","parent_id":"`+parent.String()+`","default_shelf_life_days":30,"storage_id":"`+foreign.String()+`"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	assert.Equal(t, f.storageID, f.categories.lastStorageID)
	assert.Equal(t, f.user.ID, f.categories.lastUserID)
	assert.Equal(t, "Cheese", f.categories.lastCreate.Name, "the name is trimmed before it is stored")
	require.NotNil(t, f.categories.lastCreate.ParentID)
	assert.Equal(t, parent, *f.categories.lastCreate.ParentID)
	require.NotNil(t, f.categories.lastCreate.DefaultShelfLifeDays)
	assert.Equal(t, 30, *f.categories.lastCreate.DefaultShelfLifeDays)

	assert.Contains(t, rec.Body.String(), `"default_shelf_life_days":30`)
	assert.Contains(t, rec.Body.String(), `"children":[]`)
	assert.NotContains(t, rec.Body.String(), "storage_id")

	t.Run("no shelf life means inherit", func(t *testing.T) {
		rec := f.do(http.MethodPost, f.base()+"/categories", `{"name":"Spices"}`)
		require.Equal(t, http.StatusCreated, rec.Code)
		assert.Nil(t, f.categories.lastCreate.ParentID, "no parent_id is a root")
		assert.Nil(t, f.categories.lastCreate.DefaultShelfLifeDays)
	})
}

// TestUpdateCategorySeparatesAbsentFromNull is the category counterpart of
// TestUpdateLocationSeparatesAbsentFromNull.
func TestUpdateCategorySeparatesAbsentFromNull(t *testing.T) {
	t.Parallel()

	newParent := uuid.New()

	tests := []struct {
		name            string
		body            string
		wantName        *string
		wantSetParent   bool
		wantParentIsNil bool
	}{
		{name: "parent_id omitted leaves the parent alone", body: `{"name":"Renamed"}`, wantName: ptr("Renamed")},
		{name: "parent_id null makes it a root", body: `{"parent_id":null}`, wantSetParent: true, wantParentIsNil: true},
		{name: "parent_id set re-parents", body: `{"parent_id":"` + newParent.String() + `"}`, wantSetParent: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			id := uuid.New()
			rec := f.do(http.MethodPatch, f.base()+"/categories/"+id.String(), tc.body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			patch := f.categories.lastPatch
			assert.Equal(t, id, f.categories.lastID)
			assert.Equal(t, f.storageID, f.categories.lastStorageID)
			assert.Equal(t, tc.wantSetParent, patch.SetParentID)
			if tc.wantName == nil {
				assert.Nil(t, patch.Name, "a body that never mentioned the name must not rewrite it")
			} else {
				require.NotNil(t, patch.Name)
				assert.Equal(t, *tc.wantName, *patch.Name)
			}
			if tc.wantSetParent && tc.wantParentIsNil {
				assert.Nil(t, patch.ParentID, "null means root, not unchanged")
			}
			if tc.wantSetParent && !tc.wantParentIsNil {
				require.NotNil(t, patch.ParentID)
				assert.Equal(t, newParent, *patch.ParentID)
			}
		})
	}
}

// TestUpdateCategoryRefusesAShelfLifeItWouldIgnore: the rule has its own route
// because changing it recomputes dates and reports a count. A PATCH carrying
// it must not answer 200 while leaving the old rule in force.
func TestUpdateCategoryRefusesAShelfLifeItWouldIgnore(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/categories/"+uuid.New().String(),
		`{"name":"Dairy","default_shelf_life_days":10}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotEmpty(t, errorFields(t, rec)["default_shelf_life_days"])
	assert.Zero(t, f.categories.updates, "nothing is written when part of the request is refused")
}

func TestUpdateCategoryValidatesItsInput(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{"blank name", `{"name":"  "}`, "name"},
		{"malformed parent", `{"parent_id":"nope"}`, "parent_id"},
		{"parent of the wrong type", `{"parent_id":7}`, "parent_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPatch, f.base()+"/categories/"+uuid.New().String(), tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)[tc.field])
			assert.Zero(t, f.categories.updates)
		})
	}
}

// TestCategoryStoreErrorsMapToTheSpecStatusCodes: a foreign or missing id is
// 404 and never 403 (docs/specs/03-auth-and-multi-tenancy.md), a cycle and a
// delete blocked by products are 409 — and neither conflict puts the store's
// internal wording in front of a person.
func TestCategoryStoreErrorsMapToTheSpecStatusCodes(t *testing.T) {
	t.Parallel()

	message := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.Error.Message
	}

	t.Run("a category in another storage is 404 on every write", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.categories.createErr = store.ErrNotFound
		f.categories.updateErr = store.ErrNotFound
		f.categories.deleteErr = store.ErrNotFound
		id := uuid.New().String()

		for _, rec := range []*httptest.ResponseRecorder{
			f.do(http.MethodPost, f.base()+"/categories", `{"name":"x","parent_id":"`+id+`"}`),
			f.do(http.MethodPatch, f.base()+"/categories/"+id, `{"name":"Mine now"}`),
			f.do(http.MethodDelete, f.base()+"/categories/"+id, ""),
		} {
			require.Equal(t, http.StatusNotFound, rec.Code)
			assert.Equal(t, "not_found", errorCode(t, rec))
		}
	})

	t.Run("a cycle is 409 with a message for people", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.categories.updateErr = fmt.Errorf("%w: re-parenting would create a cycle in categories", store.ErrConflict)

		rec := f.do(http.MethodPatch, f.base()+"/categories/"+uuid.New().String(),
			`{"parent_id":"`+uuid.New().String()+`"}`)

		require.Equal(t, http.StatusConflict, rec.Code)
		assert.NotContains(t, message(t, rec), "store:")
		assert.Contains(t, message(t, rec), "cannot be moved inside itself")
	})

	t.Run("deleting a category products still use is 409 with a message for people", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.categories.deleteErr = fmt.Errorf("%w: category is still used by 3 product(s)", store.ErrConflict)

		rec := f.do(http.MethodDelete, f.base()+"/categories/"+uuid.New().String(), "")

		require.Equal(t, http.StatusConflict, rec.Code)
		assert.NotContains(t, message(t, rec), "store:")
		assert.Contains(t, message(t, rec), "Move them to another category first")
	})
}

func TestDeleteCategoryAnswers204WithNoBody(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	id := uuid.New()

	rec := f.do(http.MethodDelete, f.base()+"/categories/"+id.String(), "")

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, []uuid.UUID{id}, f.categories.deleted)
	assert.Equal(t, f.storageID, f.categories.lastStorageID)
}

func TestMalformedCategoryIDIs404NotBadRequest(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	for _, tc := range []struct{ method, body string }{
		{http.MethodPatch, `{"name":"x"}`},
		{http.MethodDelete, ""},
	} {
		rec := f.do(tc.method, f.base()+"/categories/not-a-uuid", tc.body)
		require.Equal(t, http.StatusNotFound, rec.Code, tc.method)
		assert.Equal(t, "not_found", errorCode(t, rec))
	}
}

func ptr[T any](v T) *T { return &v }
