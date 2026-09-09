package store

import "errors"

// Domain errors. Handlers map these to status codes
// (docs/specs/04-backend-api-conventions.md); nothing below knows about HTTP.
var (
	// ErrNotFound covers both "no such row" and "that row belongs to another
	// storage" — deliberately the same error.
	//
	// docs/specs/03-auth-and-multi-tenancy.md requires the two to be
	// indistinguishable to a caller, because a distinguishable response
	// confirms that an id refers to a real row somewhere and lets a user probe
	// for the existence of other storages. Collapsing them here means the
	// distinction is never represented in the first place, so no handler can
	// leak it by accident and no future refactor can reintroduce the tell.
	ErrNotFound = errors.New("store: not found")

	// ErrConflict is a legal row that cannot take the requested action: a
	// location that still holds inventory, a category still referenced by a
	// product, a re-parent that would create a cycle. Maps to 409.
	ErrConflict = errors.New("store: conflict")

	// ErrValidation is a payload the model rejects on its own terms — a batch
	// decrement larger than the batch, a catalog variant pointing at another
	// variant. Maps to 422.
	ErrValidation = errors.New("store: validation failed")
)
