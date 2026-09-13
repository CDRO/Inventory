// Package ingest turns a photo into a proposal a person reviews
// (docs/specs/06-vision-shelf-ingestion.md).
//
// The flow is: the upload saves the photo and submits a job; the job sends the
// photo to the vision model, resolves each detected label through the shared
// matching service and each proposed location path against the storage's
// tree, and stores the result as the job's payload. Nothing here writes
// inventory. The proposal alone never mutates anything; only a confirm does,
// in internal/store.
package ingest

import (
	"context"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
)

// Proposal is a job's payload: what the model saw, matched and placed. It is
// what GET /jobs/{id} returns to the review UI, so everything in it is safe to
// hand to any member of the storage — and nothing in it identifies another
// storage, not even a catalog row's id.
type Proposal struct {
	// Mode is "shelf" or "product".
	Mode vision.Mode `json:"mode"`
	// LocationHintID is the shelf the upload was scoped to, if any.
	LocationHintID *uuid.UUID `json:"location_hint_id"`
	// Rows are the detections, each with the row_id a confirm must decide.
	Rows []Row `json:"rows"`
}

// Row is one detected product.
type Row struct {
	// RowID is what a confirm names this row by. Positional and stable: the
	// payload is written once and never rebuilt.
	RowID       string      `json:"row_id"`
	Label       string      `json:"label"`
	Confidence  float64     `json:"confidence"`
	Quantity    int         `json:"quantity"`
	BoundingBox *vision.Box `json:"bounding_box"`
	Match       Match       `json:"match"`
	Location    Placement   `json:"location"`
}

// Match is the matching service's answer for a label, shaped for display.
type Match struct {
	// Status is exact_match, ambiguous or new_item.
	Status     string       `json:"status"`
	Product    *ProductRef  `json:"product"`
	Candidates []ProductRef `json:"candidates"`
	// Catalog is the anonymous catalog's description when stage 2 hit —
	// display fields only, never the catalog row's id.
	Catalog *CatalogCard `json:"catalog"`
}

// ProductRef is one of this storage's own products.
type ProductRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// CatalogCard is what the catalog knows about a product. Catalog text was
// written by a stranger: the review UI renders it as text, never as HTML
// (docs/specs/02-data-model.md).
type CatalogCard struct {
	DisplayName          string   `json:"display_name"`
	CategoryPath         *string  `json:"category_path"`
	ItemType             string   `json:"item_type"`
	ImageURL             *string  `json:"image_url"`
	IconName             *string  `json:"icon_name"`
	DefaultShelfLifeDays *int     `json:"default_shelf_life_days"`
	Variants             []string `json:"variants"`
}

// Placement is where a row would go.
type Placement struct {
	// Path is the model's proposed path, root to leaf, each segment marked
	// with the existing node it matched or as proposed.
	Path []PathSegment `json:"path"`
	// LocationID is the default the review UI preselects: the node the whole
	// path resolved to, or the upload's hint when the model proposed no path.
	// Nil when part of the path does not exist yet — the reviewer then either
	// creates the proposed nodes on confirm or picks another shelf.
	LocationID *uuid.UUID `json:"location_id"`
}

// PathSegment is one step of a proposed path.
type PathSegment struct {
	Name string `json:"name"`
	// LocationID is the existing node this segment matched. Nil for a
	// proposed segment, and for every segment after the first proposed one:
	// a node cannot exist under a parent that does not.
	LocationID *uuid.UUID `json:"location_id"`
	Proposed   bool       `json:"proposed"`
}

// Matcher resolves a label to products (internal/matching).
type Matcher interface {
	MatchProductCandidates(ctx context.Context, storageID uuid.UUID, text string) (matching.Result, error)
}

// buildProposal assembles the payload from an analysis.
func buildProposal(ctx context.Context, matcher Matcher, storageID uuid.UUID, mode vision.Mode, hint *uuid.UUID, tree []store.Location, analysis *vision.Analysis) (*Proposal, error) {
	index := newTreeIndex(tree)

	out := &Proposal{Mode: mode, LocationHintID: hint, Rows: make([]Row, 0, len(analysis.Items))}
	for i, item := range analysis.Items {
		result, err := matcher.MatchProductCandidates(ctx, storageID, item.Label)
		if err != nil {
			return nil, err
		}
		out.Rows = append(out.Rows, Row{
			RowID:       strconv.Itoa(i),
			Label:       item.Label,
			Confidence:  item.Confidence,
			Quantity:    item.Quantity,
			BoundingBox: item.BoundingBox,
			Match:       displayMatch(result),
			Location:    index.place(item.ProposedLocationPath, hint),
		})
	}
	return out, nil
}

func displayMatch(result matching.Result) Match {
	out := Match{Status: string(result.Status), Candidates: []ProductRef{}}
	if result.Product != nil {
		out.Product = &ProductRef{ID: result.Product.ProductID, Name: result.Product.Name}
	}
	for _, c := range result.Candidates {
		out.Candidates = append(out.Candidates, ProductRef{ID: c.ProductID, Name: c.Name})
	}
	if c := result.Catalog; c != nil {
		// Field by field. matching.CatalogMatch carries the catalog row's id,
		// and this payload goes to the browser.
		variants := make([]string, 0, len(c.Variants))
		for _, v := range c.Variants {
			variants = append(variants, v.DisplayName)
		}
		out.Catalog = &CatalogCard{
			DisplayName:          c.DisplayName,
			CategoryPath:         c.CategoryPath,
			ItemType:             c.ItemType,
			ImageURL:             c.ImageURL,
			IconName:             c.IconName,
			DefaultShelfLifeDays: c.DefaultShelfLifeDays,
			Variants:             variants,
		}
	}
	return out
}

// treeIndex finds children by name.
type treeIndex struct {
	children map[uuid.UUID][]store.Location
	roots    []store.Location
}

func newTreeIndex(tree []store.Location) *treeIndex {
	idx := &treeIndex{children: map[uuid.UUID][]store.Location{}}
	for _, loc := range tree {
		if loc.ParentID == nil {
			idx.roots = append(idx.roots, loc)
		} else {
			idx.children[*loc.ParentID] = append(idx.children[*loc.ParentID], loc)
		}
	}
	return idx
}

// place resolves a proposed path against the tree.
//
// Segments match an existing node by name, ignoring case and surrounding
// space, walking down from the storage's top level. The first segment that
// does not match, and everything after it, is proposed. A path that resolves
// completely preselects its leaf; an empty path falls back to the upload's
// hint.
func (idx *treeIndex) place(path []string, hint *uuid.UUID) Placement {
	out := Placement{Path: make([]PathSegment, 0, len(path))}
	if len(path) == 0 {
		out.LocationID = hint
		return out
	}

	level := idx.roots
	var matched *uuid.UUID
	resolving := true
	for _, name := range path {
		seg := PathSegment{Name: name, Proposed: true}
		if resolving {
			if node := findByName(level, name); node != nil {
				id := node.ID
				seg.LocationID, seg.Proposed = &id, false
				matched = &id
				level = idx.children[id]
			} else {
				resolving = false
			}
		}
		out.Path = append(out.Path, seg)
	}
	if resolving {
		out.LocationID = matched
	}
	return out
}

func findByName(nodes []store.Location, name string) *store.Location {
	want := strings.TrimSpace(name)
	for i := range nodes {
		if strings.EqualFold(strings.TrimSpace(nodes[i].Name), want) {
			return &nodes[i]
		}
	}
	return nil
}
