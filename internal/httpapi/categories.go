package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// CategoryStore is the slice of the store the category handlers use.
type CategoryStore interface {
	CategoryTree(ctx context.Context, storageID uuid.UUID) ([]store.Category, error)
	CreateCategoryAsUser(ctx context.Context, storageID uuid.UUID, in store.NewCategory, userID uuid.UUID) (*store.Category, error)
	UpdateCategory(ctx context.Context, storageID, id uuid.UUID, patch store.CategoryPatch) (*store.Category, error)
	DeleteCategory(ctx context.Context, storageID, id uuid.UUID) error
}

// CategoryHandler serves a storage's category tree
// (docs/specs/02-data-model.md, docs/specs/08-expiration-and-classification.md):
// the list the category page and the ingestion review screen's picker
// (docs/specs/06-vision-shelf-ingestion.md) both read, and the create, rename,
// move and delete writes behind categories.html.
//
// It mirrors LocationHandler route for route, because spec 02 gives the two
// trees the same shape and the same invariants. The one route it does not
// have is the shelf-life rule: changing that is a correction that recomputes
// dates and reports how many moved, so it lives on
// PATCH .../categories/{id}/shelf-life (ExpiryHandler) rather than as one more
// field of Update.
//
// Every route is mounted behind RequireStorageMember, and every handler takes
// the storage id from the request context rather than the URL — the same
// guarantee LocationHandler documents: a category id from another storage is
// indistinguishable from a nonexistent one.
type CategoryHandler struct {
	store  CategoryStore
	errors *ErrorWriter
}

// NewCategoryHandler wires the handler to a store and the one error writer.
func NewCategoryHandler(s CategoryStore, errs *ErrorWriter) *CategoryHandler {
	return &CategoryHandler{store: s, errors: errs}
}

// categoryNode is one node of the response tree — the same shape
// locationNode uses, with the shelf-life rule in place of a description.
//
// default_shelf_life_days is always present and null when the node sets no
// rule of its own. Null is a statement here, not an absence: it means
// "inherit from the parent", which is different from 0 ("expires the day it
// arrives").
//
// storage_id is deliberately absent, for the same reason locationNode omits
// it: the filtering happens once, in the query, so there is no per-node
// value a bug could get wrong.
type categoryNode struct {
	ID                   uuid.UUID       `json:"id"`
	Name                 string          `json:"name"`
	DefaultShelfLifeDays *int            `json:"default_shelf_life_days"`
	Children             []*categoryNode `json:"children"`
}

// List serves GET /api/storages/{storage_id}/categories.
func (h *CategoryHandler) List(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	rows, err := h.store.CategoryTree(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusOK, collection[*categoryNode]{Items: nestCategories(rows)})
}

// Create serves POST /api/storages/{storage_id}/categories. Body:
// {name, parent_id, default_shelf_life_days} — parent_id and the shelf life
// both optional.
//
// A shelf life given here needs no cascade: nothing can be filed under a
// category that does not exist yet.
func (h *CategoryHandler) Create(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	var body struct {
		Name                 string          `json:"name"`
		ParentID             *string         `json:"parent_id"`
		DefaultShelfLifeDays json.RawMessage `json:"default_shelf_life_days"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	name := validateTreeNodeName(body.Name, fields)

	var parentID *uuid.UUID
	if body.ParentID != nil {
		parsed, err := uuid.Parse(*body.ParentID)
		if err != nil {
			// 422 rather than 404, for the reason LocationHandler.Create gives:
			// it says only that the input was malformed.
			fields["parent_id"] = append(fields["parent_id"], "Must be a UUID.")
		} else {
			parentID = &parsed
		}
	}

	var days *int
	if body.DefaultShelfLifeDays != nil {
		parsed, failure := parseShelfLifeDays(body.DefaultShelfLifeDays)
		if failure != nil {
			for field, messages := range failure.Fields {
				fields[field] = append(fields[field], messages...)
			}
		}
		days = parsed
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	created, err := h.store.CreateCategoryAsUser(r.Context(), storageID, store.NewCategory{
		Name:                 name,
		ParentID:             parentID,
		DefaultShelfLifeDays: days,
	}, user.ID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "category parent not in this storage or nonexistent"))
		return
	}

	writeJSON(w, http.StatusCreated, newCategoryNode(*created))
}

// Update serves PATCH /api/storages/{storage_id}/categories/{id} — a rename, a
// re-parent, or both in one call, with the same absent-versus-null parent_id
// contract as LocationHandler.Update.
//
// A re-parent recomputes the derived expiry dates under the moved subtree
// (store.UpdateCategory); like re-filing a product, that recompute is silent,
// so the response is the node alone.
func (h *CategoryHandler) Update(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	id, failure := idFromPath(r, "id", "malformed category id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		Name                 *string         `json:"name"`
		ParentID             json.RawMessage `json:"parent_id"`
		DefaultShelfLifeDays json.RawMessage `json:"default_shelf_life_days"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	patch := store.CategoryPatch{}

	if body.Name != nil {
		name := validateTreeNodeName(*body.Name, fields)
		patch.Name = &name
	}

	if body.ParentID != nil {
		patch.SetParentID = true
		var raw *string
		if err := json.Unmarshal(body.ParentID, &raw); err != nil {
			fields["parent_id"] = append(fields["parent_id"], "Must be a UUID or null.")
		} else if raw != nil {
			parsed, err := uuid.Parse(*raw)
			if err != nil {
				fields["parent_id"] = append(fields["parent_id"], "Must be a UUID or null.")
			} else {
				patch.ParentID = &parsed
			}
		}
	}

	if body.DefaultShelfLifeDays != nil {
		// Refused rather than ignored. Unknown fields are otherwise tolerated,
		// but this one names a real change a client would believe had been
		// made; silently dropping it would leave the old rule in force with a
		// 200 saying otherwise.
		fields["default_shelf_life_days"] = append(fields["default_shelf_life_days"],
			"Change the shelf life through PATCH .../categories/{id}/shelf-life, which also updates existing dates.")
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	updated, err := h.store.UpdateCategory(r.Context(), storageID, id, patch)
	if err != nil {
		// One reason for the node and the proposed parent, as in
		// LocationHandler.Update; the only conflict is a cycle.
		h.errors.WriteError(w, r, conflictAs(err,
			"category or parent not in this storage or nonexistent",
			"A category cannot be moved inside itself or one of its own subcategories."))
		return
	}

	writeJSON(w, http.StatusOK, newCategoryNode(*updated))
}

// Delete serves DELETE /api/storages/{storage_id}/categories/{id}, removing the
// node and everything beneath it.
//
// A subtree that still files any product is a 409 rather than a cascade
// (docs/specs/02-data-model.md): products.category_id is ON DELETE RESTRICT,
// and a product silently losing its category would also silently change which
// shelf-life rule its dates follow.
func (h *CategoryHandler) Delete(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	id, failure := idFromPath(r, "id", "malformed category id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if err := h.store.DeleteCategory(r.Context(), storageID, id); err != nil {
		h.errors.WriteError(w, r, conflictAs(err,
			"category not in this storage or nonexistent",
			"Products are still filed under this category or one beneath it. Move them to another category first."))
		return
	}

	writeJSON(w, http.StatusNoContent, nil)
}

// nestCategories turns CategoryTree's flat, parent-id-linked rows into a
// tree, the same algorithm nestLocations (locations.go) uses.
func nestCategories(rows []store.Category) []*categoryNode {
	byID := make(map[uuid.UUID]*categoryNode, len(rows))
	for _, row := range rows {
		byID[row.ID] = newCategoryNode(row)
	}

	roots := make([]*categoryNode, 0, len(rows))
	for _, row := range rows {
		node := byID[row.ID]
		if row.ParentID == nil {
			roots = append(roots, node)
			continue
		}
		parent, ok := byID[*row.ParentID]
		if !ok {
			// Same-storage validation on every write means a category's
			// parent is always in this same result set; showing the node as
			// a root anyway, rather than dropping it, is the choice that
			// never hides a real category from the picker.
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	return roots
}

func newCategoryNode(row store.Category) *categoryNode {
	return &categoryNode{
		ID:                   row.ID,
		Name:                 row.Name,
		DefaultShelfLifeDays: row.DefaultShelfLifeDays,
		// Never nil: the tree component reads node.children directly, and a
		// JSON null there would make every leaf a special case in the client.
		Children: []*categoryNode{},
	}
}
