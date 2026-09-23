package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MaxBarcodeLength matches product_barcodes.barcode's VARCHAR(64).
//
// The stored value is the raw decoded string, not a validated EAN: the scanner
// already verified the checksum, and a code somebody reads off a damaged label
// and types in is valid input even when it is unusual
// (docs/specs/20-barcode-recall.md).
const MaxBarcodeLength = 64

// ValidateBarcode enforces the stored charset of
// docs/specs/20-barcode-recall.md: 1-64 characters of [0-9A-Za-z._/-].
//
// **A code containing '/' is storable but not addressable by path.** Every
// route that carries a code in the URL (the lookup, the quick log, the local
// delete, the admin moderation delete) matches one path segment, so a slash
// splits it. The charset is the spec's, so it is what is validated here; the
// consequence is that such a code can be associated through a request body and
// then only ever recalled by a client that scans it into a body-carrying
// route. Real EAN/UPC codes are digits, so nothing in the intended flow is
// affected — this note exists so the gap is a known one rather than a
// discovery.
func ValidateBarcode(code string) error {
	if code == "" {
		return fmt.Errorf("%w: a barcode is required", ErrValidation)
	}
	if len(code) > MaxBarcodeLength {
		return fmt.Errorf("%w: a barcode must be at most %d characters", ErrValidation, MaxBarcodeLength)
	}
	for _, r := range code {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r == '.' || r == '_' || r == '/' || r == '-':
		default:
			return fmt.Errorf("%w: a barcode may contain only letters, digits and . _ / -", ErrValidation)
		}
	}
	return nil
}

// ProductBarcode is one storage-local association.
//
// There is no id column: (storage_id, barcode) is the primary key, because
// "this code means this product, here" is the whole of the row's identity.
type ProductBarcode struct {
	StorageID uuid.UUID
	Barcode   string
	ProductID uuid.UUID
	CreatedAt time.Time
}

// BarcodeLookup is the result of resolving a scanned code
// (docs/specs/20-barcode-recall.md, "Recall lookup").
//
// Exactly one of the two is non-nil. A local hit wins: this storage's own
// association is always more specific than the global hint, and a household
// that has named a product its own way must not be shown the catalogue's name
// for it instead.
type BarcodeLookup struct {
	// Product is this storage's own product, with the live stock sum.
	Product      *Product
	CurrentStock int
	// Catalog is the anonymous global hint, when no local association exists.
	// Only display fields of it may ever reach a response
	// (docs/specs/02-data-model.md).
	Catalog *CatalogProduct
}

// AssociateBarcode attaches a code to a product in one storage, and — when the
// product came from the catalogue — records the global hint in the same
// transaction.
//
// **No gamification contribution is written here, deliberately.** Accepting
// the capture-time offer is exactly the shape of action
// docs/specs/50-gamification-overview.md excludes: cheap, repeatable, and
// trivially inflatable by anyone who wanted to farm it. Declining and
// disabling the offer write nothing at all, so there is no counterpart to
// balance. The absence is the rule, not an oversight.
//
// A code already mapped to another product in this storage is ErrConflict. The
// conflict is local by construction — the primary key is (storage_id,
// barcode) — so the refusal cannot say anything about another storage, because
// there is nothing about another storage to say.
func (s *Store) AssociateBarcode(ctx context.Context, storageID, productID uuid.UUID, code string) (*ProductBarcode, error) {
	if err := ValidateBarcode(code); err != nil {
		return nil, err
	}

	var out *ProductBarcode
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// The same-storage check the foreign key cannot make: product_id is a
		// plain reference, so without this a caller could file a code against
		// a product in somebody else's house and then recall it from their
		// own. ErrNotFound, never a distinguishable refusal
		// (docs/specs/03-auth-and-multi-tenancy.md).
		if err := requireProductInStorage(ctx, tx, storageID, productID); err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO product_barcodes (storage_id, barcode, product_id)
			VALUES ($1, $2, $3)
			RETURNING storage_id, barcode, product_id, created_at`,
			storageID, code, productID)

		var pb ProductBarcode
		err := row.Scan(&pb.StorageID, &pb.Barcode, &pb.ProductID, &pb.CreatedAt)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return fmt.Errorf("%w: that barcode is already used by another product here", ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("store: associate barcode: %w", err)
		}
		out = &pb

		return insertCatalogBarcode(ctx, tx, storageID, productID, code)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// insertCatalogBarcode records the global hint for a product that has a
// catalogue row, inside the association's own transaction.
//
// Insert-only with ON CONFLICT DO NOTHING, for the reason catalog_products is
// (docs/specs/02-data-model.md): the first household to claim a code keeps it,
// and a rewritable global row would be a messaging channel between strangers.
// A product with no catalog_id contributes nothing — there is no anonymous
// description to point the code at.
func insertCatalogBarcode(ctx context.Context, q querier, storageID, productID uuid.UUID, code string) error {
	var catalogID *uuid.UUID
	err := q.QueryRow(ctx,
		`SELECT catalog_id FROM products WHERE id = $1 AND storage_id = $2`,
		productID, storageID).Scan(&catalogID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read product catalog id: %w", err)
	}
	if catalogID == nil {
		return nil
	}

	if _, err := q.Exec(ctx, `
		INSERT INTO catalog_barcodes (barcode, catalog_id)
		VALUES ($1, $2)
		ON CONFLICT (barcode) DO NOTHING`, code, *catalogID); err != nil {
		return fmt.Errorf("store: insert catalog barcode: %w", err)
	}
	return nil
}

// DeleteProductBarcode removes one storage-local association.
//
// The catalogue hint is untouched. It is a one-shot claim like a catalogue
// name, not a mirror of any household's current state, and deleting it here
// would let one storage revoke a global row every other storage benefits from.
func (s *Store) DeleteProductBarcode(ctx context.Context, storageID, productID uuid.UUID, code string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireProductInStorage(ctx, tx, storageID, productID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM product_barcodes WHERE storage_id = $1 AND barcode = $2 AND product_id = $3`,
			storageID, code, productID)
		if err != nil {
			return fmt.Errorf("store: delete product barcode: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ProductBarcodes lists the codes attached to one product, oldest first — the
// list the product edit screen shows (docs/specs/16-product-maintenance.md).
func (s *Store) ProductBarcodes(ctx context.Context, storageID, productID uuid.UUID) ([]ProductBarcode, error) {
	if err := requireProductInStorage(ctx, s.pool, storageID, productID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT storage_id, barcode, product_id, created_at
		  FROM product_barcodes
		 WHERE storage_id = $1 AND product_id = $2
		 ORDER BY created_at, barcode`, storageID, productID)
	if err != nil {
		return nil, fmt.Errorf("store: list product barcodes: %w", err)
	}
	defer rows.Close()

	out := make([]ProductBarcode, 0)
	for rows.Next() {
		var pb ProductBarcode
		if err := rows.Scan(&pb.StorageID, &pb.Barcode, &pb.ProductID, &pb.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan product barcode: %w", err)
		}
		out = append(out, pb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list product barcodes: %w", err)
	}
	return out, nil
}

// LookupBarcode resolves a scanned code for one storage: this storage's own
// product first, the anonymous catalogue hint second, ErrNotFound third.
//
// **No external service is consulted at any stage.** That is the whole point
// of the feature: identification of something the system has never seen stays
// vision-first, and a barcode is only ever a key into what is already known
// (docs/specs/00-overview.md's amended non-goal).
//
// The local branch is scoped by storage_id, so the same physical code in two
// storages resolves independently and neither can see the other's
// association — beyond the anonymous card both may be shown, which names no
// storage and carries no count.
func (s *Store) LookupBarcode(ctx context.Context, storageID uuid.UUID, code string) (*BarcodeLookup, error) {
	if err := ValidateBarcode(code); err != nil {
		// An unusable code is a miss, not a 422: the lookup's answer for
		// anything it cannot resolve has to be the one indistinguishable 404
		// (docs/specs/20-barcode-recall.md).
		return nil, ErrNotFound
	}

	row := s.pool.QueryRow(ctx, `
		SELECT p.id, p.storage_id, p.name, p.category_id, p.catalog_id, p.item_type,
		       p.default_shelf_life_days, p.min_stock, p.image_url, p.icon_name,
		       p.created_at, p.updated_at
		  FROM product_barcodes pb
		  JOIN products p ON p.id = pb.product_id AND p.storage_id = pb.storage_id
		 WHERE pb.storage_id = $1 AND pb.barcode = $2`, storageID, code)

	product, err := scanProduct(row)
	switch {
	case err == nil:
		stock, err := s.CurrentStock(ctx, storageID, product.ID)
		if err != nil {
			return nil, err
		}
		return &BarcodeLookup{Product: product, CurrentStock: stock}, nil
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}

	catalogRow := s.pool.QueryRow(ctx, `
		SELECT c.id, c.normalized_name, c.display_name, c.base_id, c.category_path, c.item_type,
		       c.image_url, c.icon_name, c.default_shelf_life_days, c.created_at
		  FROM catalog_barcodes cb
		  JOIN catalog_products c ON c.id = cb.catalog_id
		 WHERE cb.barcode = $1`, code)

	catalog, err := scanCatalogProduct(catalogRow)
	if err != nil {
		// ErrNotFound falls through unchanged: an unknown code and a code
		// belonging to a product this caller may not see are the same answer.
		return nil, err
	}
	return &BarcodeLookup{Catalog: catalog}, nil
}

// maxHotBarcodes bounds the instance-wide popularity list
// (docs/specs/24-barcode-hot-cache.md).
const maxHotBarcodes = 500

// HotBarcode is one row of the instance-wide popularity list.
//
// Barcode is included because the client keys its local cache by the scanned
// code — it is the thing printed on the package, not an internal id, so
// returning it does not reopen the "no ids" rule display-field responses
// otherwise follow. Everything else is exactly catalog_products' display
// fields, and scan_count itself is never one of them.
type HotBarcode struct {
	Barcode              string
	DisplayName          string
	CategoryPath         *string
	ItemType             ItemType
	ImageURL             *string
	IconName             *string
	DefaultShelfLifeDays *int
}

// HotBarcodes returns the instance-wide most-scanned barcodes, ranked by
// scan_count descending and ties broken by catalog_id for a stable order
// across calls — never by scan_count itself, which is not part of the
// returned type and must never reach a response
// (docs/specs/24-barcode-hot-cache.md).
//
// Computed from live data on every call: at household/instance scale a
// LIMIT 500 ordered scan of catalog_barcodes needs no materialized rollup or
// supporting index. Revisit only if a real deployment shows otherwise.
func (s *Store) HotBarcodes(ctx context.Context) ([]HotBarcode, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT cb.barcode, c.display_name, c.category_path, c.item_type,
		       c.image_url, c.icon_name, c.default_shelf_life_days
		  FROM catalog_barcodes cb
		  JOIN catalog_products c ON c.id = cb.catalog_id
		 ORDER BY cb.scan_count DESC, cb.catalog_id
		 LIMIT $1`, maxHotBarcodes)
	if err != nil {
		return nil, fmt.Errorf("store: hot barcodes: %w", err)
	}
	defer rows.Close()

	out := make([]HotBarcode, 0)
	for rows.Next() {
		var h HotBarcode
		var itemType string
		if err := rows.Scan(&h.Barcode, &h.DisplayName, &h.CategoryPath, &itemType,
			&h.ImageURL, &h.IconName, &h.DefaultShelfLifeDays); err != nil {
			return nil, fmt.Errorf("store: scan hot barcode: %w", err)
		}
		h.ItemType = ItemType(itemType)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: hot barcodes: %w", err)
	}
	return out, nil
}

// IncrementBarcodeScanCount records one successful scan against a catalog
// barcode's popularity counter (docs/specs/24-barcode-hot-cache.md).
//
// The single atomic UPDATE is the whole of the write — no read-modify-write
// race — and it is meant to be called after a lookup's response is already on
// the wire (see httpapi.BarcodeHandler.Lookup), never awaited by it: counting
// a scan must never add latency to the lookup a person is waiting on, and a
// failed count update must never fail the lookup itself. A barcode with no
// catalog_barcodes row — a local-only association, or a code that missed
// entirely — matches no row, so the UPDATE is a harmless no-op rather than a
// case the caller has to detect first.
func (s *Store) IncrementBarcodeScanCount(ctx context.Context, code string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE catalog_barcodes SET scan_count = scan_count + 1 WHERE barcode = $1`, code); err != nil {
		return fmt.Errorf("store: increment barcode scan count: %w", err)
	}
	return nil
}

// DeleteCatalogBarcode is admin moderation of a wrong global mapping, the
// counterpart of DeleteCatalogProduct and audited for the same reason: it
// crosses every household (docs/specs/18-operations-and-observability.md).
//
// **It never touches any storage's product_barcodes.** A household that
// scanned this code onto its own product keeps that association; what is
// removed is only the hint offered to households that have never seen the
// product. After the delete the next association may re-insert the mapping,
// which is how a bad first claim is corrected rather than frozen.
func (s *Store) DeleteCatalogBarcode(ctx context.Context, actor uuid.UUID, code string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		// The catalogue display name is read first: catalog_barcodes carries
		// no description of its own, so without this the audit row would
		// record only an opaque number.
		var displayName string
		err := tx.QueryRow(ctx, `
			SELECT c.display_name
			  FROM catalog_barcodes cb
			  JOIN catalog_products c ON c.id = cb.catalog_id
			 WHERE cb.barcode = $1`, code).Scan(&displayName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: delete catalog barcode: %w", err)
		}

		tag, err := tx.Exec(ctx, `DELETE FROM catalog_barcodes WHERE barcode = $1`, code)
		if err != nil {
			return fmt.Errorf("store: delete catalog barcode: %w", err)
		}
		// See DeleteCatalogProduct: the lookup above is not proof the delete
		// landed, and the trail must not record a moderation this request did
		// not perform.
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return writeAdminAudit(ctx, tx, actor, ActionCatalogBarcodeDeleted, code, AuditDetails{
			DisplayName: displayName,
		})
	})
}

// BarcodePrompt is a user's state for the capture-time offer
// (docs/specs/20-barcode-recall.md, "Offering a barcode at first capture").
type BarcodePrompt struct {
	// Enabled is users.barcode_prompt_enabled: whether the offer appears at
	// all, for this person, in every storage they belong to.
	Enabled bool
	// FirstTime is whether the *playful* first-time copy applies. It is true
	// only while barcode_prompt_seen_at is still NULL.
	FirstTime bool
}

// BarcodePromptState reads a user's offer state without changing it — what
// settings.html's Account section shows
// (docs/specs/14-account-self-service.md).
func (s *Store) BarcodePromptState(ctx context.Context, userID uuid.UUID) (BarcodePrompt, error) {
	var out BarcodePrompt
	var seenAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT barcode_prompt_enabled, barcode_prompt_seen_at FROM users WHERE id = $1`,
		userID).Scan(&out.Enabled, &seenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BarcodePrompt{}, ErrNotFound
	}
	if err != nil {
		return BarcodePrompt{}, fmt.Errorf("store: read barcode prompt state: %w", err)
	}
	out.FirstTime = seenAt == nil
	return out, nil
}

// SetBarcodePromptEnabled is the whole of PATCH /api/auth/barcode-prompt:
// "turn this off", and the re-enable from the profile.
//
// barcode_prompt_seen_at is deliberately not reset by re-enabling. The playful
// copy explains a feature, and somebody turning the offer back on has already
// had it explained — docs/specs/20-barcode-recall.md's "never shown twice to
// the same user" is unconditional.
func (s *Store) SetBarcodePromptEnabled(ctx context.Context, userID uuid.UUID, enabled bool) (BarcodePrompt, error) {
	var out BarcodePrompt
	var seenAt *time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE users SET barcode_prompt_enabled = $2
		 WHERE id = $1
		RETURNING barcode_prompt_enabled, barcode_prompt_seen_at`, userID, enabled).
		Scan(&out.Enabled, &seenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BarcodePrompt{}, ErrNotFound
	}
	if err != nil {
		return BarcodePrompt{}, fmt.Errorf("store: set barcode prompt enabled: %w", err)
	}
	out.FirstTime = seenAt == nil
	return out, nil
}

// MarkBarcodePromptShown records that the offer is being shown to this user
// now, and reports in the same breath whether it should be shown at all and
// whether this is their first time.
//
// **One statement, because "exactly once" is a race otherwise.** Splitting
// this into a read of barcode_prompt_seen_at and a later write would let two
// qualifying products — two tabs, or a confirm and a shopping-list accept
// seconds apart — both read NULL and both render the playful onboarding copy,
// which is precisely what docs/specs/20-barcode-recall.md forbids. The row is
// locked, the conditional UPDATE either wins or does not, and FirstTime is
// whether *this* call was the one that won. No handler discipline is involved.
//
// seen_at is not set when the offer is disabled: nothing is being shown, so
// there is nothing to remember having shown. A user who turns the offer off
// before ever seeing it still gets the explanatory copy if they turn it back
// on, which is the only reading under which that copy does its job.
func (s *Store) MarkBarcodePromptShown(ctx context.Context, userID uuid.UUID) (BarcodePrompt, error) {
	var out BarcodePrompt
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			WITH locked AS (
			    SELECT id, barcode_prompt_enabled, barcode_prompt_seen_at
			      FROM users WHERE id = $1 FOR UPDATE
			), marked AS (
			    UPDATE users u
			       SET barcode_prompt_seen_at = now()
			      FROM locked l
			     WHERE u.id = l.id
			       AND l.barcode_prompt_enabled
			       AND l.barcode_prompt_seen_at IS NULL
			    RETURNING u.id
			)
			SELECT l.barcode_prompt_enabled, EXISTS (SELECT 1 FROM marked)
			  FROM locked l`, userID).Scan(&out.Enabled, &out.FirstTime)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: mark barcode prompt shown: %w", err)
		}
		return nil
	})
	if err != nil {
		return BarcodePrompt{}, err
	}
	return out, nil
}

// repointBarcodes moves a merged product's codes onto the survivor
// (docs/specs/16-product-maintenance.md, docs/specs/20-barcode-recall.md).
//
// The delete runs first so that the survivor's own association wins any
// collision, which is what docs/specs/20-barcode-recall.md asks for. A
// straight UPDATE would instead violate (storage_id, barcode) and fail the
// whole merge.
//
// That collision is in fact unreachable today: the same primary key means two
// products in one storage cannot carry the same code in the first place, so
// there is never a duplicate for the delete to remove. It is written this way
// regardless, because the criterion is about what happens if the pair exists,
// and a merge is not the place to discover that it can.
func repointBarcodes(ctx context.Context, q querier, storageID, survivorID, sourceID uuid.UUID) error {
	if _, err := q.Exec(ctx, `
		DELETE FROM product_barcodes
		 WHERE storage_id = $1 AND product_id = $2
		   AND barcode IN (SELECT barcode FROM product_barcodes
		                    WHERE storage_id = $1 AND product_id = $3)`,
		storageID, sourceID, survivorID); err != nil {
		return fmt.Errorf("store: drop colliding merged barcodes: %w", err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE product_barcodes SET product_id = $1
		 WHERE storage_id = $2 AND product_id = $3`,
		survivorID, storageID, sourceID); err != nil {
		return fmt.Errorf("store: re-point merged barcodes: %w", err)
	}
	return nil
}

// BarcodeLogDirection is which way the quick-log sheet moves stock
// (docs/specs/20-barcode-recall.md, "Scan-and-log"). It mirrors the sticky
// capture mode: stocking up, or using up.
type BarcodeLogDirection string

const (
	BarcodeLogIn  BarcodeLogDirection = "in"
	BarcodeLogOut BarcodeLogDirection = "out"
)

// BarcodeLogInput is the confirmed contents of one quick-log sheet.
//
// **It is only ever built from an explicit confirm tap.** The scan event
// itself writes nothing anywhere — the sheet is the review step, smaller than
// a job review because there is nothing probabilistic to review, but the
// invariant that no scan or photo mutates inventory by itself holds here
// exactly as it does in specs 06 and 09.
type BarcodeLogInput struct {
	Direction BarcodeLogDirection
	// Quantity is always a positive count of units, whichever direction this
	// is. The sign is applied here, never by a caller.
	Quantity int

	// LocationID is where stock arrives, for BarcodeLogIn. Required in that
	// direction and ignored in the other.
	LocationID *uuid.UUID
	// ExpirationDate is the date the sheet showed, for BarcodeLogIn.
	ExpirationDate *time.Time
	// ExpirationEdited records that a person changed the offered date, which
	// makes it a 'user' date rather than a 'derived' one — the distinction the
	// cascade in docs/specs/08-expiration-and-classification.md depends on.
	// An untouched sheet leaves it false, and the date stays derived, so a
	// later shelf-life correction still reaches it.
	ExpirationEdited bool

	// Decrements name which batches a BarcodeLogOut applies to. Empty means
	// "the default": nearest expiry first, spilling into the next batch as
	// each is exhausted, exactly as the consumption review allocates
	// (docs/specs/09-consumption-logging.md).
	Decrements []ConsumeBatchDecrement
}

// BarcodeLogResult is what the confirm wrote.
type BarcodeLogResult struct {
	ProductID uuid.UUID
	// CreatedBatchID is the batch a BarcodeLogIn created.
	CreatedBatchID *uuid.UUID
	// TouchedBatchIDs are the batches a BarcodeLogOut reduced or emptied.
	TouchedBatchIDs []uuid.UUID
	// CurrentStock is the product's stock after the write, so the sheet can
	// close on a true number without a second round trip.
	CurrentStock int
}

// LogBarcode resolves a scanned code in this storage and writes the confirmed
// sheet, all in one transaction.
//
// **Local resolution only.** A code known solely to the catalogue is
// ErrNotFound here: the quick flow logs against a product this storage already
// has, and a catalogue hit is an offer to *create* one, which is the lookup
// route's business and a separate, deliberate act.
//
// Every write goes through createBatch or adjustBatch, which is what keeps the
// project's central invariant intact without restating it: each of them pairs
// its inventory_batches write with an inventory_logs row inside this same
// transaction. There is deliberately no INSERT or UPDATE of a batch in this
// function's own SQL.
func (s *Store) LogBarcode(ctx context.Context, storageID uuid.UUID, code string, userID *uuid.UUID, in BarcodeLogInput) (*BarcodeLogResult, error) {
	if in.Quantity < 1 {
		return nil, fmt.Errorf("%w: a quantity of at least 1 is required", ErrValidation)
	}
	if err := ValidateBarcode(code); err != nil {
		// Same reasoning as LookupBarcode: an unusable code is a miss, not a
		// complaint about the code.
		return nil, ErrNotFound
	}

	out := &BarcodeLogResult{TouchedBatchIDs: []uuid.UUID{}}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var productID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT pb.product_id
			  FROM product_barcodes pb
			  JOIN products p ON p.id = pb.product_id AND p.storage_id = pb.storage_id
			 WHERE pb.storage_id = $1 AND pb.barcode = $2`, storageID, code).Scan(&productID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: resolve barcode to log: %w", err)
		}
		out.ProductID = productID

		switch in.Direction {
		case BarcodeLogIn:
			if in.LocationID == nil {
				return fmt.Errorf("%w: stocking up needs a location", ErrValidation)
			}
			source := ExpirationDerived
			if in.ExpirationEdited {
				source = ExpirationUser
			}
			batch, err := createBatch(ctx, tx, storageID, NewBatch{
				ProductID:        productID,
				LocationID:       *in.LocationID,
				Quantity:         in.Quantity,
				ExpirationDate:   in.ExpirationDate,
				ExpirationSource: source,
				// The same reason a shopping-list line resolution writes, so
				// the quick flow's rows are indistinguishable in the ledger
				// from the equivalent spec 07 confirm.
				Reason:    ReasonPurchase,
				CreatedBy: userID,
			})
			if err != nil {
				return err
			}
			out.CreatedBatchID = &batch.ID

		case BarcodeLogOut:
			decrements := in.Decrements
			if len(decrements) == 0 {
				decrements, err = defaultDecrements(ctx, tx, storageID, productID, in.Quantity)
				if err != nil {
					return err
				}
			}
			total := 0
			for _, dec := range decrements {
				if dec.Quantity < 1 {
					return fmt.Errorf("%w: a decrement must remove at least 1 unit", ErrValidation)
				}
				total += dec.Quantity
			}
			if total != in.Quantity {
				return fmt.Errorf("%w: the chosen batches remove %d units, not %d", ErrValidation, total, in.Quantity)
			}
			for _, dec := range decrements {
				owner, err := adjustBatch(ctx, tx, storageID, dec.BatchID, -dec.Quantity, ReasonConsumption, userID)
				if err != nil {
					return err
				}
				// The same-storage check adjustBatch already made is about the
				// batch. This one is about the pairing: without it a caller
				// could scan one product's code and decrement a different
				// product's batch in the same storage.
				if owner != productID {
					return fmt.Errorf("%w: batch %s does not belong to the scanned product", ErrValidation, dec.BatchID)
				}
				out.TouchedBatchIDs = append(out.TouchedBatchIDs, dec.BatchID)
			}

		default:
			return fmt.Errorf("%w: direction must be \"in\" or \"out\"", ErrValidation)
		}

		return tx.QueryRow(ctx, `
			SELECT coalesce(sum(b.quantity), 0)
			  FROM inventory_batches b
			  JOIN products p ON p.id = b.product_id
			 WHERE b.product_id = $1 AND p.storage_id = $2`, productID, storageID).Scan(&out.CurrentStock)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// defaultDecrements allocates a decrement across a product's batches, nearest
// expiry first, spilling into the next batch as each is exhausted.
//
// It is the server-side twin of greedyAllocate in
// web/static/js/pages/consume-review.js, so the sheet a scan opens starts from
// the same allocation the consumption review would have proposed. A quantity
// larger than the product's whole stock is ErrValidation — a 422 — rather than
// a partial write: over-decrementing is the one thing
// docs/specs/09-consumption-logging.md refuses to clamp, because a clamped
// decrement records a consumption that did not happen.
func defaultDecrements(ctx context.Context, q querier, storageID, productID uuid.UUID, quantity int) ([]ConsumeBatchDecrement, error) {
	rows, err := q.Query(ctx, `
		SELECT b.id, b.quantity
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE b.product_id = $1 AND p.storage_id = $2
		 ORDER BY b.expiration_date NULLS LAST, b.created_at`, productID, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: list batches to decrement: %w", err)
	}
	defer rows.Close()

	out := []ConsumeBatchDecrement{}
	remaining := quantity
	for rows.Next() {
		if remaining <= 0 {
			break
		}
		var id uuid.UUID
		var available int
		if err := rows.Scan(&id, &available); err != nil {
			return nil, fmt.Errorf("store: scan batch to decrement: %w", err)
		}
		take := available
		if take > remaining {
			take = remaining
		}
		if take > 0 {
			out = append(out, ConsumeBatchDecrement{BatchID: id, Quantity: take})
			remaining -= take
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list batches to decrement: %w", err)
	}
	if remaining > 0 {
		return nil, fmt.Errorf("%w: only %d units are in stock, cannot remove %d",
			ErrValidation, quantity-remaining, quantity)
	}
	return out, nil
}

// CreateProductFromBarcodeHint creates this storage's own product from the
// anonymous catalogue hint a scanned code carries, and associates the code
// with it, in one transaction.
//
// # Why this cannot be done by the client
//
// docs/specs/20-barcode-recall.md says accepting a catalogue card "creates the
// storage-local product the same way a shopping-list catalog card does". That
// path (ResolveShoppingListItem's AcceptedCatalogID) is driven by a catalog
// **id** — and the card a scan returns is forbidden from carrying one
// (docs/specs/02-data-model.md): a client that knew the id could correlate
// catalogue rows across requests. So the resolution from code to catalog row
// has to happen here, server-side, where the id never leaves.
//
// The created product carries catalog_id, exactly as the shopping-list accept
// does, so the shelf-life cascade of
// docs/specs/08-expiration-and-classification.md reaches it like any other
// catalogue-derived product.
//
// image_url is deliberately not copied. The catalogue stores a provider URL,
// which must never reach a browser; the icon travels instead, and a picture
// can be chosen later from the product screen.
//
// No gamification contribution is written. Neither does the shopping-list
// catalogue accept this mirrors — its only contribution is for resolving an
// *ambiguity*, which a barcode by definition has none of.
//
// Refusals:
//   - ErrConflict when the code already resolves to a product in this storage.
//     There is nothing to create, and the conflict is local by construction —
//     the same reasoning AssociateBarcode's conflict rests on.
//   - ErrNotFound when the code carries no catalogue hint, which is the same
//     answer an unknown code gets from the lookup.
func (s *Store) CreateProductFromBarcodeHint(ctx context.Context, storageID uuid.UUID, code string) (*Product, error) {
	if err := ValidateBarcode(code); err != nil {
		return nil, ErrNotFound
	}

	var out *Product
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var taken bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM product_barcodes WHERE storage_id = $1 AND barcode = $2)`,
			storageID, code).Scan(&taken); err != nil {
			return fmt.Errorf("store: check local barcode: %w", err)
		}
		if taken {
			return fmt.Errorf("%w: that barcode already names a product here", ErrConflict)
		}

		row := tx.QueryRow(ctx, `
			SELECT c.id, c.normalized_name, c.display_name, c.base_id, c.category_path, c.item_type,
			       c.image_url, c.icon_name, c.default_shelf_life_days, c.created_at
			  FROM catalog_barcodes cb
			  JOIN catalog_products c ON c.id = cb.catalog_id
			 WHERE cb.barcode = $1`, code)
		catalog, err := scanCatalogProduct(row)
		if err != nil {
			return err
		}

		var categoryID *uuid.UUID
		if catalog.CategoryPath != nil && *catalog.CategoryPath != "" {
			id, err := ensureCategoryPath(ctx, tx, storageID, *catalog.CategoryPath)
			if err != nil {
				return err
			}
			categoryID = id
		}

		product, err := createProduct(ctx, tx, storageID, NewProduct{
			Name:                 catalog.DisplayName,
			CategoryID:           categoryID,
			CatalogID:            &catalog.ID,
			ItemType:             catalog.ItemType,
			DefaultShelfLifeDays: catalog.DefaultShelfLifeDays,
			IconName:             catalog.IconName,
		})
		if err != nil {
			return err
		}
		out = product

		if _, err := tx.Exec(ctx, `
			INSERT INTO product_barcodes (storage_id, barcode, product_id)
			VALUES ($1, $2, $3)`, storageID, code, product.ID); err != nil {
			return fmt.Errorf("store: associate barcode with created product: %w", err)
		}
		// catalog_barcodes is untouched: the hint that made this possible is
		// already there, and it is insert-only.
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
