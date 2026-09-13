package httpapi

import (
	"encoding/base64"
	"net/http"
	"strconv"

	"github.com/google/uuid"
)

// Page sizes (docs/specs/04-backend-api-conventions.md).
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// pageRequest is a parsed ?limit=&cursor= pair.
type pageRequest struct {
	Limit int
	// After is the id of the last item on the previous page, or nil for the
	// first page.
	After *uuid.UUID
}

// readPage parses the pagination query parameters.
//
// Cursors, never offsets: an offset counts rows, so a row inserted or deleted
// between two page requests shifts every later page by one and the client
// either sees an item twice or never. A cursor names the last item seen, and
// "everything after that item" does not move.
//
// A limit above the maximum is lowered to it rather than refused — a client
// asking for more than it may have is not making a mistake worth a round trip.
// A limit that is not a positive integer, or a cursor this server did not
// issue, is a 422 naming the parameter.
func readPage(r *http.Request) (pageRequest, *Failure) {
	q := r.URL.Query()
	page := pageRequest{Limit: defaultPageLimit}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return page, ValidationFailed(map[string][]string{"limit": {"Must be a positive whole number."}}, err)
		}
		page.Limit = min(n, maxPageLimit)
	}

	if raw := q.Get("cursor"); raw != "" {
		id, ok := decodeCursor(raw)
		if !ok {
			return page, ValidationFailed(map[string][]string{"cursor": {"Not a cursor from a previous page."}}, nil)
		}
		page.After = &id
	}
	return page, nil
}

// encodeCursor makes an item id into a cursor.
//
// Opaque on purpose. The cursor is an id today, but a client that learned to
// build one from an id would break the day ordering needs a second key; one
// that only ever echoes next_cursor back cannot.
func encodeCursor(id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(id[:])
}

func decodeCursor(raw string) (uuid.UUID, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) != len(uuid.UUID{}) {
		return uuid.Nil, false
	}
	id, err := uuid.FromBytes(b)
	return id, err == nil
}

// pageOf turns rows fetched with limit+1 into one page of a collection.
//
// Fetching one extra row is how the last page is detected without a count
// query: if the extra row came back there is another page, and its cursor is
// the last row actually returned. next_cursor is null exactly when the
// collection is exhausted.
func pageOf[T any](rows []T, limit int, idOf func(T) uuid.UUID) collection[T] {
	if rows == nil {
		rows = []T{}
	}
	if len(rows) <= limit {
		return collection[T]{Items: rows}
	}
	rows = rows[:limit]
	next := encodeCursor(idOf(rows[len(rows)-1]))
	return collection[T]{Items: rows, NextCursor: &next}
}
