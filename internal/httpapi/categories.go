package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// CategoryStore is the slice of the store the category listing handler uses.
type CategoryStore interface {
	CategoryTree(ctx context.Context, storageID uuid.UUID) ([]store.Category, error)
}

// CategoryHandler serves the category tree as a picker for the ingestion
// review screen (docs/specs/06-vision-shelf-ingestion.md): a new product
// created while reviewing a proposal names an existing category by id, and
// the picker needs the whole tree to offer one. Consumption logging
// (docs/specs/09-consumption-logging.md) never creates a product — "unlike
// IngestDecision there is no NewProduct alternative" — so this has no
// caller there.
//
// Every route is mounted behind RequireStorageMember, and the handler takes
// the storage id from the request context rather than the URL — the same
// guarantee LocationHandler documents.
type CategoryHandler struct {
	store  CategoryStore
	errors *ErrorWriter
}

// NewCategoryHandler wires the handler to a store and the one error writer.
func NewCategoryHandler(s CategoryStore, errs *ErrorWriter) *CategoryHandler {
	return &CategoryHandler{store: s, errors: errs}
}

// categoryNode is one node of the response tree — the same shape
// locationNode uses, minus fields categories don't have.
//
// storage_id is deliberately absent, for the same reason locationNode omits
// it: the filtering happens once, in the query, so there is no per-node
// value a bug could get wrong.
type categoryNode struct {
	ID       uuid.UUID       `json:"id"`
	Name     string          `json:"name"`
	Children []*categoryNode `json:"children"`
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
		ID:   row.ID,
		Name: row.Name,
		// Never nil: the tree component reads node.children directly, and a
		// JSON null there would make every leaf a special case in the client.
		Children: []*categoryNode{},
	}
}
