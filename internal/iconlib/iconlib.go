// Package iconlib reads the vendored, offline icon collections under
// data/ — Google's Noto Emoji set today, in IconifyJSON format — and turns
// each collection's icons into ready-to-store SVGs
// (docs/specs/42-local-icon-library.md).
//
// Nothing here makes a network call, at import time or any other time: the
// whole premise of this package is that icon data comes from a file checked
// into the repository (data/README.md), never from a live request to
// Iconify or any other host.
package iconlib

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

//go:embed data/noto.json
var embedded embed.FS

// Icon is one icon ready to become an `icons` row: name is already
// namespaced ("noto:cheese-wedge"), and SVGBody is a complete, standalone
// SVG document.
type Icon struct {
	Name    string
	SVGBody string
}

// collection is the subset of the IconifyJSON format
// (https://iconify.design/docs/types/iconify-json.html) this package reads:
// a shared default grid (Width/Height) and, per icon, its own path data plus
// optional overrides of that grid.
type collection struct {
	Prefix string                    `json:"prefix"`
	Icons  map[string]collectionIcon `json:"icons"`
	Width  int                       `json:"width"`
	Height int                       `json:"height"`
}

type collectionIcon struct {
	Body   string `json:"body"`
	Width  *int   `json:"width"`
	Height *int   `json:"height"`
	Left   *int   `json:"left"`
	Top    *int   `json:"top"`
}

// Noto parses the vendored Noto Emoji collection into one Icon per key in
// its `icons` object, named "noto:" plus the collection's own key — exactly
// the acceptance criterion in docs/specs/42-local-icon-library.md ("every
// icons.name value the base import produces is prefixed noto: and matches
// the vendored JSON's own icon keys exactly").
//
// Every key imports — aliases are not resolved into rows of their own, and
// no key is dropped for being marked hidden upstream (Iconify's own curation
// flag, not a licensing or quality signal): spec 42 deliberately does not
// hand-curate the set it vendors.
func Noto() ([]Icon, error) {
	raw, err := embedded.ReadFile("data/noto.json")
	if err != nil {
		return nil, fmt.Errorf("iconlib: read vendored noto.json: %w", err)
	}
	icons, err := parse("noto", raw)
	if err != nil {
		return nil, fmt.Errorf("iconlib: parse vendored noto.json: %w", err)
	}
	return icons, nil
}

// parse turns one IconifyJSON document into namespaced icons, sorted by name
// so that Noto's output — and therefore the order `inventory icons import`
// inserts in — is deterministic across runs and across machines.
func parse(namespace string, raw []byte) ([]Icon, error) {
	var c collection
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode IconifyJSON: %w", err)
	}

	out := make([]Icon, 0, len(c.Icons))
	for key, entry := range c.Icons {
		out = append(out, Icon{
			Name:    namespace + ":" + key,
			SVGBody: renderSVG(c, entry),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// renderSVG builds a complete, standalone SVG document from one icon's path
// data plus the collection's shared grid — the same viewBox composition
// Iconify's own renderer performs (default dimensions unless the icon
// overrides them), so a vendored icon renders identically to how Iconify
// itself would have shown it.
func renderSVG(c collection, entry collectionIcon) string {
	left := 0
	if entry.Left != nil {
		left = *entry.Left
	}
	top := 0
	if entry.Top != nil {
		top = *entry.Top
	}
	width := c.Width
	if entry.Width != nil {
		width = *entry.Width
	}
	height := c.Height
	if entry.Height != nil {
		height = *entry.Height
	}

	viewBox := strconv.Itoa(left) + " " + strconv.Itoa(top) + " " +
		strconv.Itoa(width) + " " + strconv.Itoa(height)
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="` + viewBox + `">` + entry.Body + `</svg>`
}
