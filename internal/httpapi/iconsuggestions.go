package httpapi

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// maxIconAliasLength matches icon_aliases.alias's VARCHAR(100)
// (docs/specs/40-icon-picker.md's schema; maxIconNameLength, products.go,
// mirrors the same width for icon_name).
const maxIconAliasLength = 100

// IconSuggestionStore is the slice of the store the icon-suggestions handler
// uses (docs/specs/40-icon-picker.md).
type IconSuggestionStore interface {
	SearchIconAliases(ctx context.Context, query string, limit int) ([]store.IconSuggestion, error)
	SearchIconsByName(ctx context.Context, query string, limit int) ([]store.IconSuggestion, error)
	CreateIconAlias(ctx context.Context, iconName, alias string, createdBy uuid.UUID) error
}

// IconSuggestionHandler serves the icon picker's search endpoint
// (docs/specs/40-icon-picker.md).
//
// Every result comes from this database — icon_aliases and icons, both
// vendored offline (docs/specs/42-local-icon-library.md). Nothing here
// constructs an HTTP client or reaches any host outside this deployment; the
// old design's live Iconify search call is exactly what this spec replaced.
type IconSuggestionHandler struct {
	store  IconSuggestionStore
	errors *ErrorWriter
}

// NewIconSuggestionHandler wires the handler to a store and the one error
// writer.
func NewIconSuggestionHandler(s IconSuggestionStore, errs *ErrorWriter) *IconSuggestionHandler {
	return &IconSuggestionHandler{store: s, errors: errs}
}

// maxIconSuggestions caps a search response — this spec's own replacement for
// an external provider's pagination convention, since it no longer has one.
const maxIconSuggestions = 20

// iconSuggestionDTO is one search result.
//
// SVGBody is present only for a vendored icon — trusted content, safe to
// inline directly. IconID is present only for an uploaded icon: the frontend
// renders it through GET .../icons/{id}/svg via an <img> element instead,
// the same rendering discipline ServeSVG documents (icons.go), never
// injected inline. MatchedAlias is present only for an alias hit, so the
// frontend can show why it matched ("beer -> matched via 'beer'").
type iconSuggestionDTO struct {
	IconName     string     `json:"icon_name"`
	Source       string     `json:"source"`
	SVGBody      string     `json:"svg_body,omitempty"`
	IconID       *uuid.UUID `json:"icon_id,omitempty"`
	MatchedAlias string     `json:"matched_alias,omitempty"`
}

func newIconSuggestionDTO(hit store.IconSuggestion) iconSuggestionDTO {
	dto := iconSuggestionDTO{
		IconName:     hit.IconName,
		Source:       hit.Source,
		MatchedAlias: hit.MatchedAlias,
	}
	if hit.Source == store.IconSourceVendored {
		dto.SVGBody = hit.SVGBody
	} else {
		id := hit.IconID
		dto.IconID = &id
	}
	return dto
}

// Search serves GET /api/storages/{storage_id}/icon-suggestions?query=...
//
// Local alias hits rank first, deduplicated by icon_name, then direct
// icons.name hits not already present, capped at maxIconSuggestions — the
// merge docs/specs/40-icon-picker.md's "Search endpoint" section describes.
// There is no provider-unreachable case: the only failure modes are this
// database being unreachable, already true of every route in this file, and
// a query returning zero rows, an empty rather than a failed result.
func (h *IconSuggestionHandler) Search(w http.ResponseWriter, r *http.Request) {
	if _, ok := StorageIDFrom(r.Context()); !ok {
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

	aliasHits, err := h.store.SearchIconAliases(r.Context(), query, maxIconSuggestions)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	seen := make(map[string]bool, len(aliasHits))
	suggestions := make([]iconSuggestionDTO, 0, maxIconSuggestions)
	for _, hit := range aliasHits {
		seen[hit.IconName] = true
		suggestions = append(suggestions, newIconSuggestionDTO(hit))
	}

	if len(suggestions) < maxIconSuggestions {
		nameHits, err := h.store.SearchIconsByName(r.Context(), query, maxIconSuggestions)
		if err != nil {
			h.errors.WriteError(w, r, Internal(err))
			return
		}
		for _, hit := range nameHits {
			if seen[hit.IconName] {
				continue
			}
			seen[hit.IconName] = true
			suggestions = append(suggestions, newIconSuggestionDTO(hit))
			if len(suggestions) == maxIconSuggestions {
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, struct {
		Suggestions []iconSuggestionDTO `json:"suggestions"`
	}{Suggestions: suggestions})
}

// createAliasRequest is POST .../icon-suggestions/aliases's body.
type createAliasRequest struct {
	IconName string `json:"icon_name"`
	Alias    string `json:"alias"`
}

// CreateAlias serves POST /api/storages/{storage_id}/icon-suggestions/aliases
// — the picker recording the search term a person actually typed against the
// icon they picked, so the next search for that word gets an alias hit
// ranked first (docs/specs/40-icon-picker.md, "Recording a new alias").
//
// Any authenticated storage member may call this: additive, low-risk shared
// data, not an admin action, the same reasoning icons.go's Upload already
// uses. Insert-and-ignore, so calling it twice for the same pair is a no-op,
// not an error — exactly what lets the picker call it silently, with no
// confirmation step.
func (h *IconSuggestionHandler) CreateAlias(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	var body createAliasRequest
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	iconName := strings.TrimSpace(body.IconName)
	if iconName == "" {
		fields["icon_name"] = append(fields["icon_name"], "Icon name is required.")
	} else if utf8.RuneCountInString(iconName) > maxIconNameLength {
		fields["icon_name"] = append(fields["icon_name"], "Must be at most 100 characters.")
	}
	alias := strings.TrimSpace(body.Alias)
	if alias == "" {
		fields["alias"] = append(fields["alias"], "Alias is required.")
	} else if utf8.RuneCountInString(alias) > maxIconAliasLength {
		fields["alias"] = append(fields["alias"], "Must be at most 100 characters.")
	}
	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	if err := h.store.CreateIconAlias(r.Context(), iconName, alias, user.ID); err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusNoContent, nil)
}
