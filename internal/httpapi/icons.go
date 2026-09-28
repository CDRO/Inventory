package httpapi

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// maxIconSVGBytes bounds an uploaded icon's body. SVGs this system stores
// are small, hand-sized graphics, not photos — a generous ceiling still
// leaves no honest upload anywhere near it, while bounding what one request
// can cost the same way MaxUploadBytes does for images (upload.go).
const maxIconSVGBytes = 1 << 20 // 1 MiB

// IconStore is the slice of the store the icon handlers use.
type IconStore interface {
	CreateIcon(ctx context.Context, in store.NewIcon) (*store.Icon, error)
	IconSVG(ctx context.Context, id uuid.UUID) (string, error)
}

// IconHandler serves the local icon library
// (docs/specs/42-local-icon-library.md): uploading an icon the vendored set
// has nothing close enough for, and serving one uploaded icon's SVG body.
//
// The vendored set itself is never served through this handler — 40's
// search endpoint reads the icons table directly and a vendored icon's
// svg_body is small enough to inline in that response; ServeSVG exists for
// the one case a URL is needed at all: an uploaded icon rendered through an
// <img> element rather than injected inline (see ServeSVG's own comment for
// why that distinction is the actual security control here).
type IconHandler struct {
	store  IconStore
	errors *ErrorWriter
}

// NewIconHandler wires the handler to a store and the one error writer.
func NewIconHandler(s IconStore, errs *ErrorWriter) *IconHandler {
	return &IconHandler{store: s, errors: errs}
}

// iconDTO is what Upload responds with. No svg_body: the body is fetched
// separately, through ServeSVG, by whatever element renders it.
type iconDTO struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Source string    `json:"source"`
}

// Upload serves POST /api/storages/{storage_id}/icons — a multipart form
// carrying `name` and `svg`, for the "nothing in the vendored set fits" case
// (docs/specs/42-local-icon-library.md, "Uploading a custom icon").
//
// `svg` may be a file part (a chosen .svg file) or a plain form value (SVG
// text pasted into a textarea) — both arrive in the same parsed multipart
// form, and this reads whichever is present, which is what lets one route
// serve both the "upload a file" and "paste a string" cases the spec names
// without a second content type or a second endpoint.
//
// Any authenticated storage member may upload: the same low-risk, additive-
// shared-data reasoning 40's alias endpoint uses, not an admin action.
func (h *IconHandler) Upload(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxIconSVGBytes)
	if err := r.ParseMultipartForm(maxIconSVGBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.errors.WriteError(w, r, PayloadTooLarge())
			return
		}
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"svg": {"The upload could not be read."}}, err))
		return
	}

	fields := map[string][]string{}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		fields["name"] = append(fields["name"], "Name is required.")
	} else if utf8.RuneCountInString(name) > maxIconNameLength {
		fields["name"] = append(fields["name"], "Must be at most 100 characters.")
	}

	svg, failure := readIconSVG(r)
	if failure != nil {
		for field, messages := range failure.Fields {
			fields[field] = append(fields[field], messages...)
		}
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	icon, err := h.store.CreateIcon(r.Context(), store.NewIcon{
		Name: name, SVGBody: svg, CreatedBy: user.ID,
	})
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "icon upload rejected"))
		return
	}

	writeJSON(w, http.StatusCreated, iconDTO{ID: icon.ID, Name: icon.Name, Source: icon.Source})
}

// readIconSVG reads the `svg` field, file part or plain value, and refuses
// anything that does not parse as well-formed SVG.
//
// This is a format check, not a redraw or a sanitizing pass
// (docs/specs/42-local-icon-library.md is explicit on the distinction): a
// malformed body is 422 and a well-formed one is stored exactly as
// received. What keeps an uploaded SVG from executing anything is never
// rendered inline (ServeSVG's own comment) — not a parser here trying to
// strip everything dangerous out of untrusted markup.
func readIconSVG(r *http.Request) (string, *Failure) {
	var raw []byte
	if file, _, err := r.FormFile("svg"); err == nil {
		defer func() { _ = file.Close() }()
		data, readErr := io.ReadAll(io.LimitReader(file, maxIconSVGBytes+1))
		if readErr != nil {
			return "", ValidationFailed(map[string][]string{"svg": {"The upload could not be read."}}, readErr)
		}
		if len(data) > maxIconSVGBytes {
			return "", PayloadTooLarge()
		}
		raw = data
	} else {
		raw = []byte(r.FormValue("svg"))
	}

	if len(raw) == 0 {
		return "", ValidationFailed(map[string][]string{"svg": {"An SVG file or pasted SVG text is required."}}, nil)
	}
	if !isWellFormedSVG(raw) {
		return "", ValidationFailed(map[string][]string{"svg": {"This is not a well-formed SVG document."}}, nil)
	}
	return string(raw), nil
}

// svgRoot matches any well-formed XML document whose root element's local
// name is "svg", regardless of namespace prefix — deliberately not a
// full SVG schema validation, per readIconSVG's own comment on what this
// check is and is not for.
type svgRoot struct {
	XMLName xml.Name `xml:"svg"`
}

// isWellFormedSVG reports whether raw parses as well-formed XML rooted at an
// <svg> element. xml.Unmarshal walks the whole document to find and skip
// every element even when nothing addresses their content, so this rejects
// unbalanced tags and invalid syntax anywhere in the body, not just in the
// root tag.
func isWellFormedSVG(raw []byte) bool {
	var root svgRoot
	return xml.Unmarshal(raw, &root) == nil
}

// ServeSVG serves GET /api/storages/{storage_id}/icons/{id}/svg.
//
// Icons are global (docs/specs/42-local-icon-library.md), so unlike a
// product picture this performs no per-storage ownership check on the icon
// itself — RequireStorageMember already confirmed the caller belongs to
// *some* storage, which is the whole access rule a shared, non-sensitive
// icon catalogue needs. An unknown id is the ordinary 404
// (docs/specs/03-auth-and-multi-tenancy.md's ErrNotFound convention).
//
// **This is the uploaded-icon rendering discipline the spec requires**: the
// SVG is served with its own content type and a locked-down CSP, from a
// dedicated endpoint, and the frontend renders it through an <img> element —
// never fetched and injected as inline <svg> markup, which is the only place
// embedded script in an uploaded SVG could ever run. An uploaded icon is
// untrusted content from a person using the system (not a paid provider's
// response — the distinction this project's trusted-provider posture
// already draws), and this rendering discipline, not a content sanitizer, is
// how that threat is closed.
func (h *IconHandler) ServeSVG(w http.ResponseWriter, r *http.Request) {
	id, failure := idFromPath(r, "id", "malformed icon id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	body, err := h.store.IconSVG(r.Context(), id)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "icon not found"))
		return
	}

	header := w.Header()
	header.Set("Content-Type", "image/svg+xml")
	header.Set("X-Content-Type-Options", "nosniff")
	// No inline execution context if this is ever opened directly
	// (docs/specs/07-shopping-list-reconciliation.md uses the same header
	// for the same reason on product pictures).
	header.Set("Content-Security-Policy", "default-src 'none'")
	// An icon's body never changes once stored — there is no update path,
	// only create — so a day in the browser's own cache is safe.
	header.Set("Cache-Control", "private, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
