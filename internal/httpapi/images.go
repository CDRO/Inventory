package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/CDRO/Inventory/internal/imagesearch"
)

// ImageSuggester produces image suggestions for a query.
type ImageSuggester interface {
	Suggest(ctx context.Context, query string, urlFor func(hash string) string) []imagesearch.Suggestion
}

// ImageCache serves stored suggestion bytes.
type ImageCache interface {
	Open(ctx context.Context, hash string) ([]byte, string, error)
	Touch(hash string)
}

// ImageHandler serves the image-suggestion flow of
// docs/specs/07-shopping-list-reconciliation.md.
type ImageHandler struct {
	suggester ImageSuggester
	cache     ImageCache
	errors    *ErrorWriter
}

// NewImageHandler wires the handlers to their collaborators.
func NewImageHandler(s ImageSuggester, c ImageCache, errs *ErrorWriter) *ImageHandler {
	return &ImageHandler{suggester: s, cache: c, errors: errs}
}

// maxQueryLength bounds the search text. A query longer than this is not a
// product name, and both providers would reject it anyway.
const maxQueryLength = 200

// cacheHashPattern is the exact shape of a SHA-256 hex digest.
//
// The path parameter is used to build a filename, so it is validated against
// this rather than sanitized: an allow-list of 64 hex characters cannot express
// "..", a separator, or anything else that escapes the cache directory.
var cacheHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Suggest serves GET /api/storages/{storage_id}/image-suggestions?query=...
//
// Every URL in the response is on this origin. The providers are called from
// here, their bytes are downloaded here, and the SerpAPI key never leaves this
// process — see internal/imagesearch's package doc for what hot-linking a
// provider URL would cost.
func (h *ImageHandler) Suggest(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"query": {"A query is required."}}, nil))
		return
	}
	if len(query) > maxQueryLength {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"query": {"That query is too long."}}, nil))
		return
	}

	// The URL builder is storage-scoped so the serving route stays behind the
	// same membership gate as everything else. The bytes themselves are not
	// storage-specific — the cache is shared, which is what makes a second
	// household's identical search free — but the route a browser is handed
	// still has to be one that browser is allowed to call.
	suggestions := h.suggester.Suggest(r.Context(), query, func(hash string) string {
		return "/api/storages/" + storageID.String() + "/images/" + hash
	})

	writeJSON(w, http.StatusOK, struct {
		Suggestions []imagesearch.Suggestion `json:"suggestions"`
	}{Suggestions: suggestions})
}

// Serve serves GET /api/storages/{storage_id}/images/{hash}.
// The lookup is deliberately not scoped by storage: the cache is shared, which
// is exactly what makes a second household's identical search cost no provider
// call. The membership gate on the route is what decides who may ask.
func (h *ImageHandler) Serve(w http.ResponseWriter, r *http.Request) {
	if _, ok := StorageIDFrom(r.Context()); !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	hash := chi.URLParam(r, "hash")
	if !cacheHashPattern.MatchString(hash) {
		// Not 400: a malformed hash names nothing, and saying so precisely
		// would confirm what a well-formed one looks like.
		h.errors.WriteError(w, r, NotFound("malformed cache hash"))
		return
	}

	data, contentType, err := h.cache.Open(r.Context(), hash)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "cached image not found"))
		return
	}

	// An SVG served from our own origin is same-origin script if a browser can
	// be talked into executing it. The bytes were sanitized before they were
	// stored (internal/imagesearch), and these headers are the second layer:
	// even a construct that slipped past the sanitizer can load nothing and
	// run nothing, and nosniff stops the response being re-interpreted as
	// something more permissive than its declared type.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)

	// A cached suggestion is immutable: its key is the hash of its source URL
	// and its bytes are normalized once at fetch time. Private, because the
	// route sits behind a membership check and a shared proxy must not serve
	// it to someone who has not passed one.
	w.Header().Set("Cache-Control", "private, max-age=86400")

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		// The client hung up mid-body; the status line is long gone.
		return
	}

	// Recency is refreshed after the response, never before it, so bookkeeping
	// adds no latency to an image request. The update is throttled to at most
	// once per image per hour inside the store.
	h.cache.Touch(hash)
}
