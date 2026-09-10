package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
)

// maxJSONBody caps a request body before it is decoded.
//
// Every JSON endpoint here describes a handful of fields, so the cap is far
// above any honest request and far below anything that would matter. Without
// it a caller can make the server buffer a body of their choosing, which is a
// denial of service that costs the attacker one request.
const maxJSONBody = 64 << 10 // 64 KiB

// errNoStorageInContext means a storage-scoped handler ran without
// RequireStorageMember in front of it.
//
// It is a wiring mistake, not a caller mistake, so it becomes a 500 with the
// cause in the log. The alternative — falling back to parsing the URL — is
// exactly what docs/specs/04-backend-api-conventions.md forbids, because the
// id in the path is the one nothing has validated.
var errNoStorageInContext = errors.New("httpapi: storage id missing from request context")

// collection is the shape docs/specs/04-backend-api-conventions.md gives list
// endpoints. NextCursor stays absent for collections that are returned whole —
// the location tree is one of them, since a tree paginated by cursor would
// arrive as fragments a client could not assemble.
type collection[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor,omitempty"`
}

// writeJSON renders a success response.
//
// Errors do not come through here: they go through ErrorWriter, which is the
// only thing that may produce an error envelope (errors.go). Keeping the two
// apart is what makes it impossible for a handler to hand-roll an error body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	// An encode failure at this point means the client hung up; the status line
	// is already sent, so there is nothing left to tell them.
	_ = json.NewEncoder(w).Encode(body)
}

// decodeJSON reads a request body into dst under the size cap.
//
// A malformed body is the caller's mistake, so it becomes a 422 rather than a
// 500 — the same code any other unusable payload gets.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *Failure {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return PayloadTooLarge()
		}
		return ValidationFailed(
			map[string][]string{"body": {"The request body is not valid JSON."}}, err)
	}
	return nil
}
