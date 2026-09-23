package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// fakeBarcodes stands in for the barcode half of the store.
type fakeBarcodes struct {
	// associations is keyed by storage id and code.
	associations map[uuid.UUID]map[string]uuid.UUID
	// lookup is what LookupBarcode answers, by code.
	lookup map[string]*store.BarcodeLookup
	// variants is what CatalogVariants answers.
	variants []string

	associateErr error
	deleteErr    error
	listErr      error
	lookupErr    error
	logErr       error
	acceptErr    error

	logResult *store.BarcodeLogResult
	logCalls  int
	lastLog   store.BarcodeLogInput
	lastCode  string
}

func newFakeBarcodes() *fakeBarcodes {
	return &fakeBarcodes{
		associations: map[uuid.UUID]map[string]uuid.UUID{},
		lookup:       map[string]*store.BarcodeLookup{},
	}
}

func (f *fakeBarcodes) AssociateBarcode(_ context.Context, storageID, productID uuid.UUID, code string) (*store.ProductBarcode, error) {
	if f.associateErr != nil {
		return nil, f.associateErr
	}
	if err := store.ValidateBarcode(code); err != nil {
		return nil, err
	}
	if f.associations[storageID] == nil {
		f.associations[storageID] = map[string]uuid.UUID{}
	}
	if owner, taken := f.associations[storageID][code]; taken && owner != productID {
		return nil, store.ErrConflict
	}
	f.associations[storageID][code] = productID
	return &store.ProductBarcode{
		StorageID: storageID, Barcode: code, ProductID: productID,
		CreatedAt: time.Date(2026, time.September, 23, 10, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeBarcodes) DeleteProductBarcode(_ context.Context, storageID, productID uuid.UUID, code string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if owner, ok := f.associations[storageID][code]; !ok || owner != productID {
		return store.ErrNotFound
	}
	delete(f.associations[storageID], code)
	return nil
}

func (f *fakeBarcodes) ProductBarcodes(_ context.Context, storageID, productID uuid.UUID) ([]store.ProductBarcode, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []store.ProductBarcode{}
	for code, owner := range f.associations[storageID] {
		if owner == productID {
			out = append(out, store.ProductBarcode{StorageID: storageID, Barcode: code, ProductID: owner})
		}
	}
	return out, nil
}

func (f *fakeBarcodes) LookupBarcode(_ context.Context, _ uuid.UUID, code string) (*store.BarcodeLookup, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	hit, ok := f.lookup[code]
	if !ok {
		return nil, store.ErrNotFound
	}
	return hit, nil
}

func (f *fakeBarcodes) LogBarcode(_ context.Context, _ uuid.UUID, code string, _ *uuid.UUID, in store.BarcodeLogInput) (*store.BarcodeLogResult, error) {
	f.logCalls++
	f.lastCode = code
	f.lastLog = in
	if f.logErr != nil {
		return nil, f.logErr
	}
	if f.logResult != nil {
		return f.logResult, nil
	}
	return &store.BarcodeLogResult{ProductID: uuid.New(), TouchedBatchIDs: []uuid.UUID{}, CurrentStock: 1}, nil
}

func (f *fakeBarcodes) CreateProductFromBarcodeHint(_ context.Context, storageID uuid.UUID, code string) (*store.Product, error) {
	if f.acceptErr != nil {
		return nil, f.acceptErr
	}
	if _, taken := f.associations[storageID][code]; taken {
		return nil, store.ErrConflict
	}
	hit := f.lookup[code]
	if hit == nil || hit.Catalog == nil {
		return nil, store.ErrNotFound
	}
	product := &store.Product{ID: uuid.New(), StorageID: storageID, Name: hit.Catalog.DisplayName, ItemType: hit.Catalog.ItemType}
	if f.associations[storageID] == nil {
		f.associations[storageID] = map[string]uuid.UUID{}
	}
	f.associations[storageID][code] = product.ID
	return product, nil
}

func (f *fakeBarcodes) CatalogVariants(_ context.Context, _ uuid.UUID, _ int) ([]string, error) {
	return f.variants, nil
}

// fakeBarcodePrompt stands in for the capture-time offer's user columns.
type fakeBarcodePrompt struct {
	enabled bool
	seen    bool

	stateErr error
	setErr   error
	markErr  error

	markCalls int
}

func newFakeBarcodePrompt() *fakeBarcodePrompt { return &fakeBarcodePrompt{enabled: true} }

func (f *fakeBarcodePrompt) BarcodePromptState(context.Context, uuid.UUID) (store.BarcodePrompt, error) {
	if f.stateErr != nil {
		return store.BarcodePrompt{}, f.stateErr
	}
	return store.BarcodePrompt{Enabled: f.enabled, FirstTime: !f.seen}, nil
}

func (f *fakeBarcodePrompt) SetBarcodePromptEnabled(_ context.Context, _ uuid.UUID, enabled bool) (store.BarcodePrompt, error) {
	if f.setErr != nil {
		return store.BarcodePrompt{}, f.setErr
	}
	f.enabled = enabled
	return store.BarcodePrompt{Enabled: f.enabled, FirstTime: !f.seen}, nil
}

func (f *fakeBarcodePrompt) MarkBarcodePromptShown(context.Context, uuid.UUID) (store.BarcodePrompt, error) {
	f.markCalls++
	if f.markErr != nil {
		return store.BarcodePrompt{}, f.markErr
	}
	if !f.enabled {
		return store.BarcodePrompt{Enabled: false, FirstTime: false}, nil
	}
	first := !f.seen
	f.seen = true
	return store.BarcodePrompt{Enabled: true, FirstTime: first}, nil
}

// stubDecoder is a PhotoDecoder that answers whatever the test wants, so the
// non-retention assertions below do not depend on a real barcode being
// present in a real photograph.
type stubDecoder struct {
	code string
	err  error
	seen int
}

func (s *stubDecoder) Decode(data []byte) (string, error) {
	s.seen = len(data)
	return s.code, s.err
}

// --- association ------------------------------------------------------------

func TestAssociatingABarcodeReturns201(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	productID := uuid.New()

	rec := f.do(http.MethodPost, f.base()+"/products/"+productID.String()+"/barcodes",
		`{"barcode":"4006381333931"}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	var body struct {
		Barcode   string    `json:"barcode"`
		ProductID uuid.UUID `json:"product_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "4006381333931", body.Barcode)
	assert.Equal(t, productID, body.ProductID)
}

// TestAssociatingATakenBarcodeIs409 — the conflict names nothing about another
// storage, because by construction there is nothing to name.
func TestAssociatingATakenBarcodeIs409(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	f.barcodes.associateErr = store.ErrConflict

	rec := f.do(http.MethodPost, f.base()+"/products/"+uuid.New().String()+"/barcodes",
		`{"barcode":"4006381333931"}`)

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "conflict", errorCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "storage")
}

func TestAssociatingAnUnusableBarcodeIs422(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	rec := f.do(http.MethodPost, f.base()+"/products/"+uuid.New().String()+"/barcodes",
		`{"barcode":"has spaces"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, errorFields(t, rec), "barcode")
}

// TestAssociatingRefusesAnUnknownField — a client that believed it was setting
// something has to find out (the same rule PATCH /api/auth/me follows).
func TestAssociatingRefusesAnUnknownField(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	rec := f.do(http.MethodPost, f.base()+"/products/"+uuid.New().String()+"/barcodes",
		`{"barcode":"4006381333931","product_id":"anything"}`)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestAssociatingAProductInAnotherStorageIs404(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	f.barcodes.associateErr = store.ErrNotFound

	rec := f.do(http.MethodPost, f.base()+"/products/"+uuid.New().String()+"/barcodes",
		`{"barcode":"4006381333931"}`)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

func TestListingAndDeletingAProductsBarcodes(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	productID := uuid.New()
	base := f.base() + "/products/" + productID.String() + "/barcodes"

	require.Equal(t, http.StatusCreated, f.do(http.MethodPost, base, `{"barcode":"4006381333931"}`).Code)

	rec := f.do(http.MethodGet, base, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var list struct {
		Items []struct {
			Barcode string `json:"barcode"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, "4006381333931", list.Items[0].Barcode)
	assert.Nil(t, list.NextCursor)

	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, base+"/4006381333931", "").Code)
	assert.Equal(t, http.StatusNotFound, f.do(http.MethodDelete, base+"/4006381333931", "").Code)
}

// --- lookup -----------------------------------------------------------------

func TestLookupReturnsTheLocalProduct(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	productID := uuid.New()
	f.barcodes.lookup["4006381333931"] = &store.BarcodeLookup{
		Product: &store.Product{
			ID: productID, Name: "Canned Tomatoes", ItemType: store.ItemLongShelfLife, MinStock: 2,
		},
		CurrentStock: 7,
	}

	rec := f.do(http.MethodGet, f.base()+"/barcodes/4006381333931", "")

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Product *struct {
			ProductID    uuid.UUID `json:"product_id"`
			Name         string    `json:"name"`
			CurrentStock int       `json:"current_stock"`
			MinStock     int       `json:"min_stock"`
		} `json:"product"`
		CatalogSuggestion json.RawMessage `json:"catalog_suggestion"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, body.Product)
	assert.Equal(t, productID, body.Product.ProductID)
	assert.Equal(t, 7, body.Product.CurrentStock)
	assert.Equal(t, 2, body.Product.MinStock)
	assert.JSONEq(t, "null", string(body.CatalogSuggestion), "both keys are always present")
}

// TestLookupCatalogCardCarriesDisplayFieldsOnly is the response-shape parity
// assertion the acceptance criterion asks for: the card has exactly the keys
// docs/specs/07-shopping-list-reconciliation.md's stage-2 card has, and no id,
// timestamp or storage-derived value anywhere.
func TestLookupCatalogCardCarriesDisplayFieldsOnly(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	path := "Food/Tinned"
	shelfLife := 720
	icon := "mdi:food-can"
	f.barcodes.variants = []string{"Passata"}
	f.barcodes.lookup["4006381333931"] = &store.BarcodeLookup{
		Catalog: &store.CatalogProduct{
			ID:                   uuid.New(),
			DisplayName:          "Canned Tomatoes",
			NormalizedName:       "canned tomatoes",
			CategoryPath:         &path,
			ItemType:             store.ItemLongShelfLife,
			IconName:             &icon,
			DefaultShelfLifeDays: &shelfLife,
			CreatedAt:            time.Now(),
		},
	}

	rec := f.do(http.MethodGet, f.base()+"/barcodes/4006381333931", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Product           json.RawMessage            `json:"product"`
		CatalogSuggestion map[string]json.RawMessage `json:"catalog_suggestion"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.JSONEq(t, "null", string(body.Product))

	keys := make([]string, 0, len(body.CatalogSuggestion))
	for key := range body.CatalogSuggestion {
		keys = append(keys, key)
	}
	assert.ElementsMatch(t,
		[]string{"display_name", "category_path", "item_type", "image_url", "icon_name",
			"default_shelf_life_days", "variants"},
		keys, "the anonymous card is display fields and nothing else")

	// The strings that would give the game away, checked against the raw body
	// rather than the parsed shape: a nested object could carry them too.
	assert.NotContains(t, rec.Body.String(), "created_at")
	assert.NotContains(t, rec.Body.String(), "normalized_name")
	assert.NotContains(t, rec.Body.String(), "storage")
	assert.NotContains(t, rec.Body.String(), "\"id\"")
}

// TestLookupOfAnUnknownCodeIsIndistinguishableFromAnyOther404 — the UI treats
// it as "unknown product" and falls back to the photo path; the response tells
// nobody anything else.
func TestLookupOfAnUnknownCodeIsIndistinguishableFromAnyOther404(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	unknown := f.do(http.MethodGet, f.base()+"/barcodes/4006381333931", "")
	require.Equal(t, http.StatusNotFound, unknown.Code)

	// A code in another storage's table looks exactly the same from here: the
	// fake answers ErrNotFound for anything it was not told about, which is
	// what the storage-scoped query does.
	elsewhere := f.do(http.MethodGet, f.base()+"/barcodes/9999999999999", "")
	assert.Equal(t, unknown.Code, elsewhere.Code)
	assert.JSONEq(t, unknown.Body.String(), elsewhere.Body.String())
}

// TestScanningWritesNothing is the invariant stated as a test: the lookup a
// scan performs never reaches the write path, whatever it resolves to.
func TestScanningWritesNothing(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	f.barcodes.lookup["4006381333931"] = &store.BarcodeLookup{
		Product: &store.Product{ID: uuid.New(), Name: "Beans", ItemType: store.ItemLongShelfLife},
	}

	require.Equal(t, http.StatusOK, f.do(http.MethodGet, f.base()+"/barcodes/4006381333931", "").Code)
	require.Equal(t, http.StatusNotFound, f.do(http.MethodGet, f.base()+"/barcodes/0000000000000", "").Code)

	assert.Zero(t, f.barcodes.logCalls, "only the confirm tap writes")
	assert.Equal(t, uuid.Nil, f.batches.lastBatchID, "and it never goes round the batch routes either")
}

// --- the quick-log sheet ----------------------------------------------------

func TestQuickLogInWritesOnConfirm(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	locationID := uuid.New()
	batchID := uuid.New()
	f.barcodes.logResult = &store.BarcodeLogResult{
		ProductID: uuid.New(), CreatedBatchID: &batchID, TouchedBatchIDs: []uuid.UUID{}, CurrentStock: 4,
	}

	rec := f.do(http.MethodPost, f.base()+"/barcodes/4006381333931/log",
		`{"direction":"in","quantity":4,"location_id":"`+locationID.String()+`"}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, 1, f.barcodes.logCalls)
	assert.Equal(t, "4006381333931", f.barcodes.lastCode)
	assert.Equal(t, store.BarcodeLogIn, f.barcodes.lastLog.Direction)
	assert.Equal(t, 4, f.barcodes.lastLog.Quantity)
	require.NotNil(t, f.barcodes.lastLog.LocationID)
	assert.Equal(t, locationID, *f.barcodes.lastLog.LocationID)
	assert.False(t, f.barcodes.lastLog.ExpirationEdited, "an untouched date stays derived")
}

// TestQuickLogMarksAnEditedDateAsTheUsersOwn — the derived/user distinction
// the expiry cascade depends on, carried across the HTTP boundary.
func TestQuickLogMarksAnEditedDateAsTheUsersOwn(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	rec := f.do(http.MethodPost, f.base()+"/barcodes/4006381333931/log",
		`{"direction":"in","quantity":1,"location_id":"`+uuid.New().String()+`","expiration_date":"2027-03-01"}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.True(t, f.barcodes.lastLog.ExpirationEdited)
	require.NotNil(t, f.barcodes.lastLog.ExpirationDate)
	assert.Equal(t, "2027-03-01", f.barcodes.lastLog.ExpirationDate.Format(time.DateOnly))
}

func TestQuickLogOutPassesTheChosenDecrementsThrough(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	batchID := uuid.New()

	rec := f.do(http.MethodPost, f.base()+"/barcodes/4006381333931/log",
		`{"direction":"out","quantity":2,"decrements":[{"batch_id":"`+batchID.String()+`","quantity":2}]}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Len(t, f.barcodes.lastLog.Decrements, 1)
	assert.Equal(t, batchID, f.barcodes.lastLog.Decrements[0].BatchID)
	assert.Equal(t, 2, f.barcodes.lastLog.Decrements[0].Quantity)
}

func TestQuickLogRefusesABadBody(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	cases := map[string]struct {
		body  string
		field string
	}{
		"unknown direction": {`{"direction":"sideways","quantity":1}`, "direction"},
		"zero quantity":     {`{"direction":"out","quantity":0}`, "quantity"},
		"no location in":    {`{"direction":"in","quantity":1}`, "location_id"},
		"bad date":          {`{"direction":"in","quantity":1,"location_id":"` + uuid.New().String() + `","expiration_date":"soon"}`, "expiration_date"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(http.MethodPost, f.base()+"/barcodes/4006381333931/log", tc.body)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Contains(t, errorFields(t, rec), tc.field)
		})
	}
	assert.Zero(t, f.barcodes.logCalls, "a refused body never reaches the store")
}

func TestQuickLogAgainstAnUnknownCodeIs404(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	f.barcodes.logErr = store.ErrNotFound

	rec := f.do(http.MethodPost, f.base()+"/barcodes/4006381333931/log",
		`{"direction":"out","quantity":1}`)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// TestQuickLogHonoursIdempotencyKeyReplay — every write here carries the
// client contract's replay semantics (docs/specs/12-client-api-contract.md).
func TestQuickLogHonoursIdempotencyKeyReplay(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	body := `{"direction":"out","quantity":1}`
	key := uuid.NewString()

	first := f.doWithHeaders(http.MethodPost, f.base()+"/barcodes/4006381333931/log", body,
		map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, first.Code)

	second := f.doWithHeaders(http.MethodPost, f.base()+"/barcodes/4006381333931/log", body,
		map[string]string{"Idempotency-Key": key})

	assert.Equal(t, first.Code, second.Code)
	assert.JSONEq(t, first.Body.String(), second.Body.String())
	assert.Equal(t, 1, f.barcodes.logCalls, "a replay must not write twice")
}

// TestAcceptingACatalogCardCreatesTheProductAndAttachesTheCode — the server
// resolves the code to its catalogue row, because the card the client was
// shown is forbidden from carrying that id.
func TestAcceptingACatalogCardCreatesTheProductAndAttachesTheCode(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	f.barcodes.lookup["4006381333931"] = &store.BarcodeLookup{
		Catalog: &store.CatalogProduct{
			ID: uuid.New(), DisplayName: "Canned Tomatoes", ItemType: store.ItemLongShelfLife,
		},
	}

	rec := f.do(http.MethodPost, f.base()+"/barcodes/4006381333931/product", "")

	require.Equal(t, http.StatusCreated, rec.Code)
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body, "product_id")
	assert.JSONEq(t, `"Canned Tomatoes"`, string(body["name"]))
	assert.NotContains(t, rec.Body.String(), "catalog_id", "the catalogue id never leaves the server")

	// The code now resolves locally, so accepting it again has nothing to do.
	again := f.do(http.MethodPost, f.base()+"/barcodes/4006381333931/product", "")
	assert.Equal(t, http.StatusConflict, again.Code)
}

func TestAcceptingACodeWithNoCatalogHintIs404(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	rec := f.do(http.MethodPost, f.base()+"/barcodes/4006381333931/product", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// --- the decode fallback ----------------------------------------------------

// multipartImage builds a one-file multipart body under the given field name.
func multipartImage(t *testing.T, field string, data []byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile(field, "photo.jpg")
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return writer.FormDataContentType(), buf.Bytes()
}

// diskSnapshot lists every file under the paths that must never gain one
// during a decode: the two image areas of
// docs/specs/04-backend-api-conventions.md, and the operating system's temp
// directory — the one a multipart parser would spill into, and the one an
// acceptance test written only against the first two would miss.
func diskSnapshot(t *testing.T, dirs ...string) []string {
	t.Helper()
	var found []string
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		require.NoError(t, filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				// A directory that vanished under us is not this test's
				// business; anything unreadable simply is not a new file.
				return nil //nolint:nilerr // see comment
			}
			if !entry.IsDir() {
				found = append(found, path)
			}
			return nil
		}))
	}
	return found
}

func decodeDirs(t *testing.T) []string {
	t.Helper()
	// TMPDIR is redirected at a fresh directory so this assertion is about
	// this test's own process and cannot be disturbed by anything else in the
	// container.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	return []string{"/data/uploads", "/data/cache", tmp}
}

// TestDecodeWritesNoFileOnSuccess is the acceptance criterion's first half.
func TestDecodeWritesNoFileOnSuccess(t *testing.T) {
	dirs := decodeDirs(t)
	decoder := &stubDecoder{code: "4006381333931"}
	f := newAPIFixture(t, func(d *httpapi.Deps) { d.BarcodeDecoder = decoder })

	before := diskSnapshot(t, dirs...)
	contentType, body := multipartImage(t, "image", bytes.Repeat([]byte{0x42}, 512*1024))
	rec := f.upload(http.MethodPost, f.base()+"/barcodes/decode", contentType, body)

	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Barcode string `json:"barcode"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "4006381333931", out.Barcode)
	assert.Equal(t, 512*1024, decoder.seen, "the whole part reaches the decoder, in memory")

	assert.ElementsMatch(t, before, diskSnapshot(t, dirs...),
		"the photograph must not be written anywhere, success included")
	assert.Empty(t, f.photos.files, "and it is not a job photo")
	assert.Empty(t, f.pictures.files, "nor a product picture")
}

// TestDecodeWritesNoFileOnFailure is the other half: "success or failure
// alike".
func TestDecodeWritesNoFileOnFailure(t *testing.T) {
	dirs := decodeDirs(t)
	f := newAPIFixture(t, func(d *httpapi.Deps) {
		d.BarcodeDecoder = &stubDecoder{err: errors.New("nothing decodable")}
	})

	before := diskSnapshot(t, dirs...)
	contentType, body := multipartImage(t, "image", bytes.Repeat([]byte{0x42}, 512*1024))
	rec := f.upload(http.MethodPost, f.base()+"/barcodes/decode", contentType, body)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "validation_failed", errorCode(t, rec))
	assert.ElementsMatch(t, before, diskSnapshot(t, dirs...))
}

// TestDecodeRefusesAnEmptyResult — never a guessed or empty-string result.
func TestDecodeRefusesAnEmptyResult(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t, func(d *httpapi.Deps) { d.BarcodeDecoder = &stubDecoder{code: ""} })

	contentType, body := multipartImage(t, "image", []byte("pretend this is a photo"))
	rec := f.upload(http.MethodPost, f.base()+"/barcodes/decode", contentType, body)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"barcode"`)
}

func TestDecodeRefusesAMissingFile(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t, func(d *httpapi.Deps) { d.BarcodeDecoder = &stubDecoder{code: "4006381333931"} })

	contentType, body := multipartImage(t, "not-the-image", []byte("x"))
	rec := f.upload(http.MethodPost, f.base()+"/barcodes/decode", contentType, body)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, errorFields(t, rec), "image")
}

func TestDecodeRefusesAnOversizedUpload(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t, func(d *httpapi.Deps) { d.BarcodeDecoder = &stubDecoder{code: "4006381333931"} })

	contentType, body := multipartImage(t, "image", bytes.Repeat([]byte{0x42}, (8<<20)+1024))
	rec := f.upload(http.MethodPost, f.base()+"/barcodes/decode", contentType, body)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// --- the capture-time offer -------------------------------------------------

func TestBarcodePromptStateIsReadable(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	rec := f.do(http.MethodGet, "/api/auth/barcode-prompt", "")

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Enabled   bool `json:"enabled"`
		FirstTime bool `json:"first_time"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body.Enabled)
	assert.True(t, body.FirstTime)
	assert.Zero(t, f.barcodePrompt.markCalls, "reading the setting is not being offered anything")
}

// TestShowingTheOfferMarksItSeenExactlyOnce — the first-time copy is never
// shown twice to the same user.
func TestShowingTheOfferMarksItSeenExactlyOnce(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	first := f.do(http.MethodPost, "/api/auth/barcode-prompt/shown", "")
	require.Equal(t, http.StatusOK, first.Code)
	assert.Contains(t, first.Body.String(), `"first_time":true`)

	second := f.do(http.MethodPost, "/api/auth/barcode-prompt/shown", "")
	require.Equal(t, http.StatusOK, second.Code)
	assert.Contains(t, second.Body.String(), `"first_time":false`)
}

// TestTurningTheOfferOffIsAPatchOnItsOwnRoute — not a field on spec 14's
// PATCH /api/auth/me, which refuses it outright.
func TestTurningTheOfferOffIsAPatchOnItsOwnRoute(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	rec := f.do(http.MethodPatch, "/api/auth/barcode-prompt", `{"enabled":false}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"enabled":false`)

	shown := f.do(http.MethodPost, "/api/auth/barcode-prompt/shown", "")
	require.Equal(t, http.StatusOK, shown.Code)
	assert.Contains(t, shown.Body.String(), `"enabled":false`, "the offer never appears again")

	onMe := f.do(http.MethodPatch, "/api/auth/me", `{"barcode_prompt_enabled":false}`)
	assert.Equal(t, http.StatusUnprocessableEntity, onMe.Code,
		"the profile patch has no such field and says so")
}

func TestBarcodePromptPatchRefusesABodyWithoutEnabled(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	assert.Equal(t, http.StatusUnprocessableEntity,
		f.do(http.MethodPatch, "/api/auth/barcode-prompt", `{}`).Code)
	assert.Equal(t, http.StatusUnprocessableEntity,
		f.do(http.MethodPatch, "/api/auth/barcode-prompt", `{"enable":true}`).Code)
}

// TestBarcodePromptRoutesRequireASession — they act on the row the session
// names, so without one there is nothing to act on.
func TestBarcodePromptRoutesRequireASession(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	for _, route := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/auth/barcode-prompt", ""},
		{http.MethodPatch, "/api/auth/barcode-prompt", `{"enabled":true}`},
		{http.MethodPost, "/api/auth/barcode-prompt/shown", ""},
	} {
		rec := f.anonymous(route.method, route.path, route.body)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, route.path)
	}
}

// --- admin moderation -------------------------------------------------------

// TestAdminCatalogBarcodeDeleteIsHiddenFromNonAdmins — the whole admin area
// answers 404, identically, whether the caller is not an admin or the route
// does not exist.
func TestAdminCatalogBarcodeDeleteIsHiddenFromNonAdmins(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	real := f.do(http.MethodDelete, "/api/admin/catalog-barcodes/4006381333931", "")
	require.Equal(t, http.StatusNotFound, real.Code)

	invented := f.do(http.MethodDelete, "/api/admin/catalog-barcodes-that-do-not-exist/x", "")
	assert.Equal(t, real.Code, invented.Code)
	assert.JSONEq(t, real.Body.String(), invented.Body.String())
}

// TestAdminDeletingACatalogBarcodeIsAudited — moderation crosses every
// household, so it leaves a trail (docs/specs/18-operations-and-observability.md).
func TestAdminDeletingACatalogBarcodeIsAudited(t *testing.T) {
	t.Parallel()
	f := newAdminFixture(t)
	f.auth.catalogBarcodes["4006381333931"] = "Canned Tomatoes"

	rec := f.do(http.MethodDelete, "/api/admin/catalog-barcodes/4006381333931", "")

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, f.auth.catalogBarcodes, "4006381333931")

	entries := f.auth.auditEntries()
	require.NotEmpty(t, entries)
	last := entries[len(entries)-1]
	assert.Equal(t, store.ActionCatalogBarcodeDeleted, last.Action)
	assert.Equal(t, "4006381333931", last.Target)
}

func TestAdminDeletingAnUnknownCatalogBarcodeIs404(t *testing.T) {
	t.Parallel()
	f := newAdminFixture(t)

	rec := f.do(http.MethodDelete, "/api/admin/catalog-barcodes/4006381333931", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// upload issues a multipart request carrying the fixture's session cookie.
func (f *apiFixture) upload(method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: f.session.ID})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// doWithHeaders is do plus extra request headers — Idempotency-Key, here.
func (f *apiFixture) doWithHeaders(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: f.session.ID})
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}
