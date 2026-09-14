package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/gamification"
)

// IngestDecision is the reviewer's verdict on one proposed row
// (docs/specs/06-vision-shelf-ingestion.md, docs/specs/09-consumption-logging.md).
type IngestDecision struct {
	RowID  string
	Accept bool

	// Exactly one of ProductID and NewProduct is set on an accepted row.
	ProductID  *uuid.UUID
	NewProduct *NewIngestProduct

	Quantity int

	// Exactly one of LocationID and NewLocation is set on an accepted row.
	LocationID  *uuid.UUID
	NewLocation *NewIngestLocation

	// ExpirationEdited says the reviewer typed the date, which makes it a
	// user date — including a deliberate "no expiry" when ExpirationDate is
	// nil. Otherwise the default is accepted and the batch gets a derived date
	// (docs/specs/08-expiration-and-classification.md).
	ExpirationEdited bool
	ExpirationDate   *time.Time
}

// NewIngestProduct is a product the reviewer created while confirming.
type NewIngestProduct struct {
	Name       string
	CategoryID *uuid.UUID
	ItemType   ItemType
}

// NewIngestLocation is a location path the model proposed that did not exist
// yet: Names, root to leaf, below ParentID (nil for the storage's top level).
type NewIngestLocation struct {
	ParentID *uuid.UUID
	Names    []string
}

// IngestResult is what a confirm wrote.
type IngestResult struct {
	BatchIDs         []uuid.UUID
	ProductsCreated  int
	LocationsCreated int
}

// ConfirmIngestion applies a reviewed proposal, all or nothing.
//
// Everything happens in one transaction, in the order the spec lists it: new
// products (each also inserted into the anonymous catalog, insert-only, with no
// image), newly referenced locations, one batch per accepted row with its
// vision_ingestion log row, and finally the job marked consumed. A crash
// anywhere leaves the job exactly as it was — still done, still in the inbox —
// and inventory untouched; there is no half-confirmed state
// (docs/specs/09-consumption-logging.md).
//
// Refusals:
//   - ErrNotFound for a job, product, category or location that is not in
//     this storage — the same answer as one that does not exist.
//   - ErrConflict for a job that is not done: still pending, failed, or
//     already consumed by an earlier confirm. That last one is what makes a
//     double submit harmless.
//   - ErrValidation when the decisions do not name exactly the rows the
//     proposal issued. A missing row is not an implicit rejection: that would
//     be the one way an item could vanish without anyone deciding it should.
func (s *Store) ConfirmIngestion(ctx context.Context, storageID, jobID uuid.UUID, userID *uuid.UUID, decisions []IngestDecision) (*IngestResult, error) {
	result := &IngestResult{BatchIDs: []uuid.UUID{}}

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var status JobStatus
		var kind JobKind
		var payload []byte
		err := tx.QueryRow(ctx, `
			SELECT status, kind, payload FROM jobs WHERE id = $1 AND storage_id = $2 FOR UPDATE`,
			jobID, storageID).Scan(&status, &kind, &payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock ingestion job: %w", err)
		}
		if status != JobDone {
			return fmt.Errorf("%w: job is %s, not done", ErrConflict, status)
		}

		ids := make([]string, len(decisions))
		for i, d := range decisions {
			ids[i] = d.RowID
		}
		if err := matchRowIDs(payload, ids); err != nil {
			return err
		}

		proposedProducts, err := proposedExactMatches(payload)
		if err != nil {
			return err
		}

		newProducts := map[string]uuid.UUID{}
		treeLocked := false

		for _, d := range decisions {
			if !d.Accept {
				continue
			}

			productID, created, err := resolveIngestProduct(ctx, tx, storageID, d, newProducts)
			if err != nil {
				return err
			}
			if created {
				result.ProductsCreated++
			}

			// The model proposed a confident, single product for this row, and
			// the reviewer ended up filing the batch against a different one
			// (or a brand-new product): that is the correction spec 51 pays
			// more than acceptance for, because it is what keeps the data
			// truthful (docs/specs/51-gamification-scoring.md).
			if userID != nil {
				if proposed, hadExactMatch := proposedProducts[d.RowID]; hadExactMatch && proposed != productID {
					if err := recordContribution(ctx, tx, storageID, *userID, gamification.KindAICorrection, &productID); err != nil {
						return err
					}
				}
			}

			locationID := d.LocationID
			if d.NewLocation != nil {
				if !treeLocked {
					if err := lockStorageTree(ctx, tx, storageID); err != nil {
						return err
					}
					treeLocked = true
				}
				id, n, err := ensureLocationPath(ctx, tx, storageID, *d.NewLocation)
				if err != nil {
					return err
				}
				locationID = &id
				result.LocationsCreated += n
			}
			if locationID == nil {
				return fmt.Errorf("%w: row %s has no location", ErrValidation, d.RowID)
			}

			source := ExpirationDerived
			if d.ExpirationEdited {
				source = ExpirationUser
			}
			batch, err := createBatch(ctx, tx, storageID, NewBatch{
				ProductID:        productID,
				LocationID:       *locationID,
				Quantity:         d.Quantity,
				ExpirationDate:   d.ExpirationDate,
				ExpirationSource: source,
				Reason:           ReasonVisionIngestion,
				CreatedBy:        userID,
			})
			if err != nil {
				return err
			}
			result.BatchIDs = append(result.BatchIDs, batch.ID)
		}

		// first_shelf unlocks on a shelf-photo job specifically — "your first
		// shelf-photo ingestion" (docs/specs/52-gamification-quests-and-ui.md)
		// — not a single product photo, which is also confirmed through this
		// same endpoint with the same ReasonVisionIngestion. Checked here,
		// where the job's kind is in scope, rather than in
		// evaluateLedgerAchievements (internal/store/achievements.go), which
		// runs per batch created inside createBatch and has no view of which
		// job produced it.
		if userID != nil && kind == JobShelfIngestion && len(result.BatchIDs) > 0 {
			if err := unlockAchievement(ctx, tx, storageID, *userID, string(gamification.AchFirstShelf)); err != nil {
				return err
			}
		}

		return ConsumeJob(ctx, tx, storageID, jobID)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// proposedExactMatches reads, per row_id, the product the model proposed
// with high enough confidence to preselect (internal/ingest.Match.Status ==
// "exact_match") — the baseline a reviewer's final decision is compared
// against to detect a correction (docs/specs/51-gamification-scoring.md).
//
// This package cannot import internal/ingest for its Proposal type: that
// package already imports internal/store, and Go does not allow the cycle.
// The struct below decodes the same JSON shape but names only the fields
// this comparison needs, the same way matchRowIDs above does for row_id.
func proposedExactMatches(payload []byte) (map[string]uuid.UUID, error) {
	var proposal struct {
		Rows []struct {
			RowID string `json:"row_id"`
			Match struct {
				Status  string `json:"status"`
				Product *struct {
					ID uuid.UUID `json:"id"`
				} `json:"product"`
			} `json:"match"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(payload, &proposal); err != nil {
		return nil, fmt.Errorf("store: decode stored proposal: %w", err)
	}

	out := make(map[string]uuid.UUID, len(proposal.Rows))
	for _, r := range proposal.Rows {
		if r.Match.Status == "exact_match" && r.Match.Product != nil {
			out[r.RowID] = r.Match.Product.ID
		}
	}
	return out, nil
}

// matchRowIDs checks that rowIDs names every row of the stored proposal
// exactly once, and nothing else.
//
// Shared by ConfirmIngestion and ConfirmConsumption
// (docs/specs/09-consumption-logging.md): both proposals are a job payload
// with a top-level "rows" array of {row_id, ...}, and both confirm bodies must
// decide every row exactly once.
func matchRowIDs(payload []byte, rowIDs []string) error {
	var proposal struct {
		Rows []struct {
			RowID string `json:"row_id"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(payload, &proposal); err != nil {
		return fmt.Errorf("store: decode stored proposal: %w", err)
	}

	issued := make(map[string]bool, len(proposal.Rows))
	for _, r := range proposal.Rows {
		issued[r.RowID] = true
	}

	seen := make(map[string]bool, len(rowIDs))
	for _, id := range rowIDs {
		if !issued[id] {
			return fmt.Errorf("%w: row %q is not part of this proposal", ErrValidation, id)
		}
		if seen[id] {
			return fmt.Errorf("%w: row %q is decided twice", ErrValidation, id)
		}
		seen[id] = true
	}
	if len(seen) != len(issued) {
		return fmt.Errorf("%w: every row needs a decision; %d of %d were decided", ErrValidation, len(seen), len(issued))
	}
	return nil
}

// resolveIngestProduct returns the product an accepted row files stock
// against, creating it if the reviewer typed a new one.
//
// Two rows naming the same new product in one confirm create it once. A
// reviewer who corrected two detections of the same jar to "Grandma's
// chutney" means one product with two batches, not two products that happen
// to share a name.
func resolveIngestProduct(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, d IngestDecision, created map[string]uuid.UUID) (uuid.UUID, bool, error) {
	if d.ProductID != nil {
		if err := requireProductInStorage(ctx, tx, storageID, *d.ProductID); err != nil {
			return uuid.Nil, false, err
		}
		return *d.ProductID, false, nil
	}
	if d.NewProduct == nil {
		return uuid.Nil, false, fmt.Errorf("%w: row %s has no product", ErrValidation, d.RowID)
	}

	key := NormalizeCatalogName(d.NewProduct.Name)
	if id, ok := created[key]; ok {
		return id, false, nil
	}

	var categoryPath *string
	if d.NewProduct.CategoryID != nil {
		if err := requireSameStorage(ctx, tx, treeCategories, storageID, *d.NewProduct.CategoryID); err != nil {
			return uuid.Nil, false, err
		}
		path, err := categoryPathOf(ctx, tx, *d.NewProduct.CategoryID)
		if err != nil {
			return uuid.Nil, false, err
		}
		categoryPath = &path
	}

	// Name, category path and item type only. ImageURL stays nil whatever the
	// product's own picture turns out to be: an ingestion photo was taken
	// inside someone's home and is never published (docs/specs/02-data-model.md).
	catalog, err := insertCatalogProduct(ctx, tx, NewCatalogProduct{
		DisplayName:  d.NewProduct.Name,
		CategoryPath: categoryPath,
		ItemType:     d.NewProduct.ItemType,
	})
	if err != nil {
		return uuid.Nil, false, err
	}

	product, err := createProduct(ctx, tx, storageID, NewProduct{
		Name:       d.NewProduct.Name,
		CategoryID: d.NewProduct.CategoryID,
		CatalogID:  &catalog.ID,
		ItemType:   d.NewProduct.ItemType,
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	created[key] = product.ID
	return product.ID, true, nil
}

// categoryPathOf renders a category's ancestry as "Food > Dairy", the form the
// catalog stores. Names only: the catalog never learns a storage's ids.
func categoryPathOf(ctx context.Context, tx pgx.Tx, categoryID uuid.UUID) (string, error) {
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT id, parent_id, name, 0 AS depth FROM categories WHERE id = $1
			UNION ALL
			SELECT c.id, c.parent_id, c.name, chain.depth + 1
			  FROM categories c JOIN chain ON c.id = chain.parent_id
			 WHERE chain.depth < 64
		)
		SELECT name FROM chain ORDER BY depth DESC`, categoryID)
	if err != nil {
		return "", fmt.Errorf("store: category path: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("store: scan category path: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("store: category path: %w", err)
	}
	return strings.Join(names, " > "), nil
}

// ensureLocationPath walks Names below ParentID, reusing a node that already
// has the name and creating the rest. It returns the leaf and how many nodes
// it created.
//
// Reuse is by case-insensitive name among siblings, so two rows proposing
// "Basement > Right Shelf" in one confirm share the nodes the first created,
// and a path the user has since created by hand is found rather than
// duplicated. The caller holds the storage's tree lock.
func ensureLocationPath(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, in NewIngestLocation) (uuid.UUID, int, error) {
	if len(in.Names) == 0 {
		return uuid.Nil, 0, fmt.Errorf("%w: a new location needs at least one name", ErrValidation)
	}
	if in.ParentID != nil {
		if err := requireSameStorage(ctx, tx, treeLocations, storageID, *in.ParentID); err != nil {
			return uuid.Nil, 0, err
		}
	}

	parent := in.ParentID
	created := 0
	for _, raw := range in.Names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return uuid.Nil, 0, fmt.Errorf("%w: location names must not be blank", ErrValidation)
		}

		var existing uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id FROM locations
			 WHERE storage_id = $1 AND parent_id IS NOT DISTINCT FROM $2 AND lower(name) = lower($3)
			 ORDER BY created_at, id
			 LIMIT 1`, storageID, parent, name).Scan(&existing)
		if err == nil {
			parent = &existing
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, 0, fmt.Errorf("store: find location: %w", err)
		}

		id, err := newID()
		if err != nil {
			return uuid.Nil, 0, err
		}
		loc, err := insertLocation(ctx, tx, storageID, id, NewLocation{ParentID: parent, Name: name})
		if err != nil {
			return uuid.Nil, 0, err
		}
		parent = &loc.ID
		created++
	}
	return *parent, created, nil
}
