package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"
)

// DeltaSinceParam is the query parameter that turns a list request into a
// delta request (docs/specs/12-client-api-contract.md).
const DeltaSinceParam = "updated_since"

// deltaCollection is what a list endpoint answers when updated_since is
// present: the same items the full list would carry, narrowed to what changed,
// plus what went away and the cursor for next time.
//
// It is a separate envelope from collection rather than three more fields on
// it. A request without updated_since gets byte-identical output to before —
// the PWA reads these endpoints and never sends the parameter — so adding
// delta sync cannot change what the browser sees. Additive, exactly as the
// versioning rules in spec 12 require.
type deltaCollection[T any] struct {
	Items []T `json:"items"`
	// Deleted is ids, not rows: the row is gone, and an id is everything a
	// client needs to drop it. Never null — an empty delta is an empty array,
	// so a client can iterate it without a special case.
	Deleted []uuid.UUID `json:"deleted"`
	// SyncedAt is the instant this delta was taken, to send back as the next
	// updated_since.
	//
	// Without it delta sync does not work at all. None of these endpoints
	// serialize updated_at on their items, so a client has nothing else to
	// compute its next cursor from, and using its own clock would make every
	// sync wrong by the skew between the phone and the NAS — in the direction
	// that silently skips changes. The server's clock is the only one that
	// orders these rows, so the server hands it back.
	SyncedAt time.Time `json:"synced_at"`
	// NextCursor is present for the same reason collection carries it: the
	// envelope is documented to have the key, and a client that stops when it
	// is null must be able to tell null from absent. These three endpoints
	// return their collections whole, so it is always null today.
	NextCursor *string `json:"next_cursor"`
}

// readDeltaSince parses ?updated_since=<RFC3339>, returning nil when the
// caller did not ask for a delta.
//
// A malformed value is 422 naming the parameter, the same answer readPage
// gives for an unusable limit or cursor — it says the request was malformed
// without saying anything about what the storage contains. That distinction
// only stays safe because the storage gate runs in front of this: a delta
// request against a storage the caller cannot see is refused by
// RequireStorageMember with the usual 404 and never reaches a handler, so the
// 422 can never become a tell that a storage exists.
//
// RFC3339 only, with no fallbacks. Accepting a date without a time, or a
// timestamp without a zone, would mean guessing a zone on the client's behalf
// and silently shifting the window by hours.
func readDeltaSince(r *http.Request) (*time.Time, *Failure) {
	raw := r.URL.Query().Get(DeltaSinceParam)
	if raw == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, ValidationFailed(map[string][]string{
			DeltaSinceParam: {"Must be an RFC 3339 timestamp, e.g. 2026-09-21T14:30:00Z."},
		}, err)
	}
	return &at, nil
}
