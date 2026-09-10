package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// maxLocationName matches locations.name VARCHAR(255) in the schema
// (docs/specs/02-data-model.md). Checking it here turns a name one character
// too long into a 422 naming the field, rather than a driver error the caller
// sees as a 500.
const maxLocationName = 255

// LocationStore is the slice of the store the location handlers use. It is an
// interface so the handlers can be tested without a database.
type LocationStore interface {
	LocationTree(ctx context.Context, storageID uuid.UUID) ([]store.Location, error)
	CreateLocation(ctx context.Context, storageID uuid.UUID, in store.NewLocation) (*store.Location, error)
	UpdateLocation(ctx context.Context, storageID, id uuid.UUID, patch store.LocationPatch) (*store.Location, error)
	DeleteLocation(ctx context.Context, storageID, id uuid.UUID) error
}

// LocationHandler serves the location tree
// (docs/specs/06-vision-shelf-ingestion.md).
//
// Every route is mounted behind RequireStorageMember, and every handler takes
// the storage id from the request context rather than the URL. That is the
// whole reason a location id from another storage is indistinguishable from a
// nonexistent one: the store resolves each id against a storage the caller was
// already proven to belong to, and answers ErrNotFound for anything else.
type LocationHandler struct {
	store  LocationStore
	errors *ErrorWriter
}

// NewLocationHandler wires the handlers to a store and the one error writer.
func NewLocationHandler(s LocationStore, errs *ErrorWriter) *LocationHandler {
	return &LocationHandler{store: s, errors: errs}
}

// locationNode is one node of the response tree.
//
// storage_id is deliberately absent. The spec requires that no node from
// another storage appear at any depth; not serializing the field at all means
// there is no per-node value a bug could get wrong, and the filtering happens
// once, in the query. The test for this asserts on the wire format, because
// that is the thing a client could actually read.
type locationNode struct {
	ID          uuid.UUID       `json:"id"`
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Children    []*locationNode `json:"children"`
}

// List serves GET /api/storages/{storage_id}/locations.
func (h *LocationHandler) List(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	rows, err := h.store.LocationTree(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusOK, collection[*locationNode]{Items: nestLocations(rows)})
}

// Create serves POST /api/storages/{storage_id}/locations.
func (h *LocationHandler) Create(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	var body struct {
		Name        string  `json:"name"`
		Description *string `json:"description"`
		ParentID    *string `json:"parent_id"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	name := validateLocationName(body.Name, fields)

	var parentID *uuid.UUID
	if body.ParentID != nil {
		parsed, err := uuid.Parse(*body.ParentID)
		if err != nil {
			// A parent id that is not a uuid cannot name a location in this
			// storage, and answering 422 rather than 404 here says only that
			// the caller sent something malformed — it does not reveal whether
			// any well-formed id would have been found.
			fields["parent_id"] = append(fields["parent_id"], "Must be a UUID.")
		} else {
			parentID = &parsed
		}
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	created, err := h.store.CreateLocation(r.Context(), storageID, store.NewLocation{
		Name:        name,
		Description: body.Description,
		ParentID:    parentID,
	})
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "location parent not in this storage or nonexistent"))
		return
	}

	writeJSON(w, http.StatusCreated, newLocationNode(*created))
}

// Update serves PATCH /api/storages/{storage_id}/locations/{id} — a rename, a
// re-parent, or both in one call.
//
// The body distinguishes an absent field from an explicit null: omitting
// parent_id leaves the node where it is, while sending null makes it a root.
// Collapsing the two would make it impossible to move a node to the top of the
// tree without also rewriting its name.
func (h *LocationHandler) Update(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	id, failure := locationIDFromPath(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		Name        *string         `json:"name"`
		Description json.RawMessage `json:"description"`
		ParentID    json.RawMessage `json:"parent_id"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	patch := store.LocationPatch{}

	if body.Name != nil {
		name := validateLocationName(*body.Name, fields)
		patch.Name = &name
	}

	if body.Description != nil {
		patch.SetDescription = true
		var description *string
		if err := json.Unmarshal(body.Description, &description); err != nil {
			fields["description"] = append(fields["description"], "Must be a string or null.")
		}
		patch.Description = description
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

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	updated, err := h.store.UpdateLocation(r.Context(), storageID, id, patch)
	if err != nil {
		// One reason covers the node and the proposed parent on purpose. Both
		// are ErrNotFound by the time they reach here, and the store dropped
		// the distinction so that nothing downstream could reintroduce it.
		//
		// The only conflict this call can raise is a cycle, so the message can
		// name it exactly.
		h.errors.WriteError(w, r, conflictAs(err,
			"location or parent not in this storage or nonexistent",
			"A location cannot be moved inside itself or one of its own children."))
		return
	}

	writeJSON(w, http.StatusOK, newLocationNode(*updated))
}

// Delete serves DELETE /api/storages/{storage_id}/locations/{id}.
//
// A location that still holds inventory — directly or anywhere beneath it — is
// a 409 rather than a cascade: the rows would be taken out with the subtree,
// leaving stock that physically exists with nowhere to be.
func (h *LocationHandler) Delete(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	id, failure := locationIDFromPath(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if err := h.store.DeleteLocation(r.Context(), storageID, id); err != nil {
		// The only conflict a delete can raise is stock still sitting under the
		// node, so the message can say what to do about it.
		h.errors.WriteError(w, r, conflictAs(err,
			"location not in this storage or nonexistent",
			"This location still holds inventory. Move or clear it first."))
		return
	}

	writeJSON(w, http.StatusNoContent, nil)
}

// conflictAs maps a store error, replacing the message a 409 would otherwise
// carry with text written for a person.
//
// FromStoreError puts the store's own wrapped error string in `message`, which
// is the right thing for a log and the wrong thing for a form: "store:
// conflict: re-parenting would create a cycle in locations" is not something to
// put in front of a household member trying to tidy a pantry. The internal text
// moves to Reason, so it still reaches the log and still appears in dev — the
// serializer decides that, as always, not this function.
func conflictAs(err error, notFoundReason, message string) *Failure {
	failure := FromStoreError(err, notFoundReason)
	if failure != nil && failure.Status == http.StatusConflict {
		failure.Reason = err.Error()
		failure.Message = message
	}
	return failure
}

// locationIDFromPath parses {id}, answering 404 for anything unparseable.
//
// A malformed id gets the same answer as a well-formed id belonging to someone
// else. Returning 400 here would tell a prober that their guess was at least
// the right shape, which is the beginning of the enumeration the 404 rule
// exists to prevent (docs/specs/03-auth-and-multi-tenancy.md).
func locationIDFromPath(r *http.Request) (uuid.UUID, *Failure) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, NotFound("malformed location id")
	}
	return id, nil
}

// validateLocationName trims and bounds a name, recording any problem under
// "name" in fields.
func validateLocationName(raw string, fields map[string][]string) string {
	name := strings.TrimSpace(raw)
	switch {
	case name == "":
		// A whitespace-only name renders as an invisible node in the tree: it
		// is there, it holds inventory, and there is nothing to click.
		fields["name"] = append(fields["name"], "A name is required.")
	case utf8.RuneCountInString(name) > maxLocationName:
		fields["name"] = append(fields["name"], "Must be at most 255 characters.")
	}
	return name
}

// nestLocations turns the flat, storage-filtered row set into the nested shape
// the tree component renders (docs/specs/05-frontend-pwa-foundations.md).
//
// It links parents in a second pass rather than trusting the row order.
// LocationTree orders by created_at, which puts a parent first only until
// somebody re-parents an old node under a newer one — after that the child
// sorts ahead of its parent, and a single-pass build would drop the subtree.
func nestLocations(rows []store.Location) []*locationNode {
	byID := make(map[uuid.UUID]*locationNode, len(rows))
	for _, row := range rows {
		byID[row.ID] = newLocationNode(row)
	}

	roots := make([]*locationNode, 0, len(rows))
	for _, row := range rows {
		node := byID[row.ID]
		if row.ParentID == nil {
			roots = append(roots, node)
			continue
		}
		parent, ok := byID[*row.ParentID]
		if !ok {
			// The parent is not in this storage's rows. Same-storage validation
			// on every write means this should not happen; showing the node as
			// a root anyway is the choice that never loses a shelf that holds
			// real inventory.
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	return roots
}

func newLocationNode(row store.Location) *locationNode {
	return &locationNode{
		ID:          row.ID,
		Name:        row.Name,
		Description: row.Description,
		// Never nil: the tree component reads node.children directly, and a
		// JSON null there would make every leaf a special case in the client.
		Children: []*locationNode{},
	}
}
