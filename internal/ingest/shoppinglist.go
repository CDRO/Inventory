package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/jobs"
	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
)

// msgNoLines is what a reviewer reads when the photo was accepted but nothing
// on it read as a list. Like every other message here it says what to do next,
// because it is read in the inbox with no other context.
//
// A photo with no lines is deliberately a failed job rather than an empty
// shopping list: an empty list is a row somebody has to find and discard, and
// it would claim the photo had been read when it had not.
const msgNoLines = "No shopping-list lines could be read from that photo. Try again with a clearer photo of the list."

// ShoppingListPayload is a shopping-list photo job's payload: the list its
// lines became. Unlike a shelf or consumption job, this job holds no proposal
// to review — the lines are matched and written as a real shopping list by the
// job itself, because docs/specs/07-shopping-list-reconciliation.md says
// processing "continues identically to the text path", and the text path
// creates the list outright. The resolution screen
// (docs/specs/07-shopping-list-reconciliation.md, "Resolution UI per state")
// is where the reviewing actually happens.
type ShoppingListPayload struct {
	ShoppingListID uuid.UUID `json:"shopping_list_id"`
}

// StartShoppingList saves a photographed shopping list and submits its job —
// docs/specs/07-shopping-list-reconciliation.md's `source: "photo"` ingestion,
// which had never been built.
//
// It is Start's shape with a different reading of the photo: the model is
// asked to read the image as a list from the start (vision.ModeShoppingList)
// rather than to look for shelved products and mention a list if it sees one.
// Somebody who uploads through this endpoint has already said what the photo
// is, so there is no mismatch to surface
// (docs/specs/41-mixed-photo-classification.md, "No reverse check").
func (s *Service) StartShoppingList(ctx context.Context, u Upload) (*store.Job, error) {
	if u.Kind != store.JobShoppingListPhoto {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedKind, u.Kind)
	}

	if err := s.photos.Save(u.Filename, u.Image); err != nil {
		return nil, err
	}

	createdBy := u.CreatedBy
	job, err := s.runner.Submit(ctx, store.NewJob{
		StorageID:     u.StorageID,
		Kind:          u.Kind,
		CreatedBy:     &createdBy,
		ImageFilename: &u.Filename,
	}, s.listWork(u.StorageID, u.Filename, &createdBy))
	if err != nil {
		if rmErr := s.photos.Remove(u.Filename); rmErr != nil {
			s.log.WarnContext(ctx, "removing the photo of a job that was never created failed", slog.Any("err", rmErr))
		}
		return nil, err
	}
	return job, nil
}

// listWork is one shopping-list photo job: read the photo, ask the model for
// its lines, then run each line through the shared matching service and write
// the list — the same three stages, in the same order, that a typed list goes
// through (docs/specs/07-shopping-list-reconciliation.md, "Matching service").
func (s *Service) listWork(storageID uuid.UUID, filename string, createdBy *uuid.UUID) jobs.Work {
	return func(ctx context.Context) (json.RawMessage, error) {
		image, err := s.photos.Read(filename)
		if errors.Is(err, os.ErrNotExist) {
			return nil, &jobs.UserError{Message: msgPhotoMissing, Err: err}
		}
		if err != nil {
			return nil, err
		}

		model, err := s.models.EffectiveModel(ctx)
		if err != nil {
			return nil, err
		}

		analysis, err := s.analyzer.Analyze(ctx, model, vision.ModeShoppingList, image, mimeTypeOf(filename))
		switch {
		case errors.Is(err, vision.ErrModelNotFound):
			s.models.Invalidate()
			return nil, &jobs.UserError{Message: msgModelUnavailable, Err: err}
		case errors.Is(err, vision.ErrMalformedResponse):
			return nil, &jobs.UserError{Message: msgUnreadable, Err: err}
		case err != nil:
			return nil, err
		}
		if len(analysis.ShoppingListLines) == 0 {
			return nil, &jobs.UserError{Message: msgNoLines}
		}

		items, _, err := MatchLines(ctx, s.matcher, storageID, analysis.ShoppingListLines)
		if err != nil {
			return nil, err
		}

		list, _, err := s.store.CreateShoppingList(ctx, storageID, store.SourcePhoto, createdBy, items)
		if err != nil {
			return nil, err
		}
		return json.Marshal(ShoppingListPayload{ShoppingListID: list.ID})
	}
}

// MatchLines classifies each raw line the way ingestion does, so that a
// photographed list, a reclassified shelf photo and a typed list all reach
// shopping_list_items through the same matching service and the same parsing
// of "eggs x2" (docs/specs/07-shopping-list-reconciliation.md).
//
// The per-line matching results ride back alongside the rows because the two
// synchronous paths answer 201 with the resolution UI's choices already
// rendered (catalog cards, candidates), and recomputing them would be a second
// pass over the same matcher. The job path simply ignores them.
//
// It is exported because the reclassification endpoint
// (docs/specs/41-mixed-photo-classification.md) matches lines that were
// extracted by an earlier job rather than by this one, and must do it
// identically — two implementations of "each line through the matching
// service" would be two places for the classification to drift.
func MatchLines(ctx context.Context, matcher Matcher, storageID uuid.UUID, lines []string) ([]store.NewShoppingListItem, []matching.Result, error) {
	items := make([]store.NewShoppingListItem, 0, len(lines))
	results := make([]matching.Result, 0, len(lines))
	for _, line := range lines {
		text, _ := matching.ParseLine(line)

		result, err := matcher.MatchProductCandidates(ctx, storageID, text)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, result)

		item := store.NewShoppingListItem{
			RawText: line,
			Status:  store.ShoppingListItemStatus(result.Status),
		}
		if result.Product != nil {
			item.MatchedProductID = &result.Product.ProductID
		}
		items = append(items, item)
	}
	return items, results, nil
}
