// Package consume turns a "using up" photo into a decrement proposal a person
// reviews (docs/specs/09-consumption-logging.md).
//
// It mirrors internal/ingest's shape — upload, job, AI proposal, editable
// review, explicit confirm — but the direction is reversed: an accepted row
// decrements batches this storage already has rather than creating one, and
// matching stops after internal/matching's stage 1. Consumption never
// creates a product, so there is nothing for a catalog hit or an external
// image search to attach a picture to; reaching either stage here would be
// wasted work at best and a stray external call at worst.
package consume

import (
	"context"
	"strconv"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/vision"
)

// Proposal is a consumption job's payload: what the model saw and matched
// against this storage's own products. It is what GET /jobs/{id} returns to
// the review screen.
type Proposal struct {
	Rows []Row `json:"rows"`
}

// Row is one detected item to be used up.
type Row struct {
	// RowID is what a confirm names this row by. Positional and stable: the
	// payload is written once and never rebuilt.
	RowID      string  `json:"row_id"`
	Label      string  `json:"label"`
	Confidence float64 `json:"confidence"`
	// Quantity is the AI-detected starting suggestion for how many units were
	// used up — always a suggestion, never applied without passing through
	// the editable review step.
	Quantity    int         `json:"quantity"`
	BoundingBox *vision.Box `json:"bounding_box"`
	Match       Match       `json:"match"`
}

// Match is internal/matching's stage-1-only answer for a label.
type Match struct {
	// Status is exact_match, ambiguous or new_item. new_item here reads as
	// "unrecognized" on the review screen: consumption never creates a
	// product, so a reviewer must correct it to an existing one or reject the
	// row.
	Status     string       `json:"status"`
	Product    *ProductRef  `json:"product"`
	Candidates []ProductRef `json:"candidates"`
}

// ProductRef is one of this storage's own products.
type ProductRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// Matcher resolves a label against this storage's own products only
// (internal/matching, stage 1).
type Matcher interface {
	MatchLocalProduct(ctx context.Context, storageID uuid.UUID, text string) (matching.Result, error)
}

// buildProposal assembles the payload from an analysis.
func buildProposal(ctx context.Context, matcher Matcher, storageID uuid.UUID, analysis *vision.Analysis) (*Proposal, error) {
	out := &Proposal{Rows: make([]Row, 0, len(analysis.Items))}
	for i, item := range analysis.Items {
		result, err := matcher.MatchLocalProduct(ctx, storageID, item.Label)
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
	return out
}
