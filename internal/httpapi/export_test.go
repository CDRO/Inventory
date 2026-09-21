package httpapi_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/store"
)

// readArchive unpacks the response body, failing the test if it is not a
// readable ZIP. That check is half the point: a truncated archive that a
// reader cannot open is the failure mode this endpoint most has to avoid.
func readArchive(t *testing.T, body []byte) map[string][]byte {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err, "the response must be a readable ZIP")

	files := map[string][]byte{}
	for _, entry := range reader.File {
		f, err := entry.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(f)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		files[entry.Name] = data
	}
	return files
}

// productImageURLFor is the address the server records for a product picture
// in permanent storage, spelled out here rather than imported: the export is
// tested from outside the package, and a test that reused the production
// helper could not catch it changing shape.
func productImageURLFor(storageID uuid.UUID, name string) string {
	return "/api/storages/" + storageID.String() + "/product-images/" + name
}

// jpegWithGPSAndEXIF builds a JPEG carrying both an APP1 EXIF block and a COM
// segment with a location in it — a photo as a phone actually produces one.
func jpegWithGPSAndEXIF(t *testing.T, gps string) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 90, A: 255})
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 95}))
	data := encoded.Bytes()
	require.Equal(t, byte(0xD8), data[1], "encoded jpeg must start with SOI")

	// A minimal TIFF block with an orientation tag, which is what the EXIF
	// marker bytes ride in on.
	tiff := []byte{
		'I', 'I',
		0x2A, 0x00,
		0x08, 0x00, 0x00, 0x00,
		0x01, 0x00,
		0x12, 0x01,
		0x03, 0x00,
		0x01, 0x00, 0x00, 0x00,
		0x06, 0x00, 0x00, 0x00, // orientation 6: rotate 90° clockwise
		0x00, 0x00, 0x00, 0x00,
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	exifSeg := append([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
	comment := append([]byte{0xFF, 0xFE, 0x00, byte(len(gps) + 2)}, gps...)

	var out bytes.Buffer
	out.Write(data[:2]) // SOI
	out.Write(exifSeg)
	out.Write(comment)
	out.Write(data[2:])
	return out.Bytes()
}

// TestExportProducesTheDocumentedArchive covers the format acceptance criteria
// of docs/specs/15-backup-restore-and-export.md in one pass: the ZIP parses,
// export.json parses, its format field is exactly inventory-export/1, trees
// are nested the way their GET endpoints nest them, products carry
// current_stock, and the download is named after the storage and the day.
func TestExportProducesTheDocumentedArchive(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	shelf := uuid.New()
	drawer := uuid.New()
	dairy := uuid.New()
	cheese := uuid.New()
	productID := uuid.New()
	batchID := uuid.New()
	who := "Rita Renter"
	expires := time.Date(2027, time.March, 4, 0, 0, 0, 0, time.UTC)

	f.exports.data = &store.StorageExport{
		Storage: store.Storage{ID: f.storageID, Name: "Kitchen Pantry"},
		Locations: []store.Location{
			{ID: shelf, StorageID: f.storageID, Name: "Shelf"},
			{ID: drawer, StorageID: f.storageID, ParentID: &shelf, Name: "Drawer"},
		},
		Categories: []store.Category{
			{ID: dairy, StorageID: f.storageID, Name: "Dairy"},
			{ID: cheese, StorageID: f.storageID, ParentID: &dairy, Name: "Cheese"},
		},
		Products: []store.ExportProduct{
			{ID: productID, Name: "Gruyère", CategoryID: &cheese, ItemType: store.ItemPerishable,
				MinStock: 2, CurrentStock: 5},
		},
		Batches: []store.Batch{
			{ID: batchID, ProductID: productID, LocationID: drawer, Quantity: 5,
				ExpirationDate: &expires, ExpirationSource: "derived"},
		},
		Logs: []store.ExportLog{
			{ID: uuid.New(), ProductID: productID, BatchID: &batchID, ChangeQty: 5,
				Reason: store.ReasonPurchase, CreatedBy: &who},
		},
		ShoppingLists: []store.ExportShoppingList{
			{ID: uuid.New(), Source: store.SourceText, CreatedBy: &who,
				Items: []store.ShoppingListItem{{ID: uuid.New(), RawText: "cheese", Status: store.ItemNewItem}}},
		},
	}

	rec := f.do(http.MethodGet, f.base()+"/export", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"),
		"a household's whole inventory is never cached anywhere")

	disposition := rec.Header().Get("Content-Disposition")
	assert.Contains(t, disposition, "inventory-export-kitchen-pantry-")
	assert.Contains(t, disposition, time.Now().UTC().Format(time.DateOnly)+".zip")

	files := readArchive(t, rec.Body.Bytes())
	require.Contains(t, files, "export.json")

	var doc struct {
		Format     string `json:"format"`
		ExportedAt string `json:"exported_at"`
		Storage    struct {
			Name string `json:"name"`
		} `json:"storage"`
		Locations []struct {
			Name     string `json:"name"`
			Children []struct {
				Name string `json:"name"`
			} `json:"children"`
		} `json:"locations"`
		Categories []struct {
			Name     string `json:"name"`
			Children []struct {
				Name string `json:"name"`
			} `json:"children"`
		} `json:"categories"`
		Products []struct {
			Name         string  `json:"name"`
			CurrentStock int     `json:"current_stock"`
			MinStock     int     `json:"min_stock"`
			ImageFile    *string `json:"image_file"`
		} `json:"products"`
		Batches []struct {
			ExpirationDate *string `json:"expiration_date"`
		} `json:"batches"`
		Logs []struct {
			CreatedBy *string `json:"created_by"`
		} `json:"logs"`
		ShoppingLists []struct {
			Items []struct {
				RawText string `json:"raw_text"`
			} `json:"items"`
		} `json:"shopping_lists"`
	}
	require.NoError(t, json.Unmarshal(files["export.json"], &doc), "export.json must parse")

	assert.Equal(t, "inventory-export/1", doc.Format,
		"the format string is the contract with whatever reads this archive later")
	assert.Equal(t, httpapi.ExportFormat, doc.Format)
	assert.NotEmpty(t, doc.ExportedAt)
	assert.Equal(t, "Kitchen Pantry", doc.Storage.Name)

	require.Len(t, doc.Locations, 1, "trees are exported nested, like their GET endpoints")
	assert.Equal(t, "Shelf", doc.Locations[0].Name)
	require.Len(t, doc.Locations[0].Children, 1)
	assert.Equal(t, "Drawer", doc.Locations[0].Children[0].Name)

	require.Len(t, doc.Categories, 1)
	require.Len(t, doc.Categories[0].Children, 1)
	assert.Equal(t, "Cheese", doc.Categories[0].Children[0].Name)

	require.Len(t, doc.Products, 1)
	assert.Equal(t, "Gruyère", doc.Products[0].Name)
	assert.Equal(t, 5, doc.Products[0].CurrentStock, "products include current_stock")
	assert.Nil(t, doc.Products[0].ImageFile, "no picture, no reference to one")

	require.Len(t, doc.Batches, 1)
	require.NotNil(t, doc.Batches[0].ExpirationDate)
	assert.Equal(t, "2027-03-04", *doc.Batches[0].ExpirationDate,
		"an expiry is a calendar date, not an instant with a timezone applied to it")

	require.Len(t, doc.Logs, 1)
	require.NotNil(t, doc.Logs[0].CreatedBy)
	assert.Equal(t, "Rita Renter", *doc.Logs[0].CreatedBy,
		"a ledger row names a person, not a user id from an instance that may be gone")

	require.Len(t, doc.ShoppingLists, 1)
	require.Len(t, doc.ShoppingLists[0].Items, 1)
	assert.Equal(t, "cheese", doc.ShoppingLists[0].Items[0].RawText)

	assert.Equal(t, f.storageID, f.exports.lastID, "the export reads the storage the gate validated")
}

// TestExportArchiveCarriesNoEXIFOrGPSMetadata is the acceptance criterion that
// the archive itself is clean.
//
// Images are stripped at upload (docs/specs/04-backend-api-conventions.md), so
// in principle the export has nothing left to do — and that is exactly why the
// test is worth having. It asserts the end state of the whole chain rather
// than trusting the step upstream: a real JPEG carrying an EXIF block and a
// COM segment with coordinates in it, through the same images.ProductImage
// call that produces a stored product picture, out through the export, and
// checked in the bytes that are actually inside the ZIP. An export is the one
// artifact designed to leave the house, so the check belongs on the artifact.
func TestExportArchiveCarriesNoEXIFOrGPSMetadata(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	const gps = "GPS 47.3769 N 8.5417 E"
	original := jpegWithGPSAndEXIF(t, gps)
	require.Contains(t, string(original), gps, "the fixture must start out dirty")
	require.True(t, bytes.Contains(original, []byte("Exif\x00\x00")), "the fixture must start out dirty")

	// The same call the product-picture path makes: strip, then re-encode from
	// pixels, so nothing of the original's metadata can survive.
	stored, err := images.ProductImage(original, nil)
	require.NoError(t, err)

	name := uuid.New().String() + ".jpg"
	require.NoError(t, f.pictures.Save(name, stored.Data))

	productID := uuid.New()
	imageURL := productImageURLFor(f.storageID, name)
	f.exports.data = &store.StorageExport{
		Storage: store.Storage{ID: f.storageID, Name: "Kitchen"},
		Products: []store.ExportProduct{
			{ID: productID, Name: "Gruyère", ImageURL: &imageURL, CurrentStock: 1},
		},
	}

	rec := f.do(http.MethodGet, f.base()+"/export", "")
	require.Equal(t, http.StatusOK, rec.Code)

	files := readArchive(t, rec.Body.Bytes())
	entry := "images/" + productID.String() + ".jpg"
	require.Contains(t, files, entry, "the product's picture travels with the archive")

	packed := files[entry]
	assert.False(t, bytes.Contains(packed, []byte("Exif\x00\x00")),
		"an EXIF block must not be in the archive")
	assert.False(t, bytes.Contains(packed, []byte("eXIf")),
		"a PNG EXIF chunk must not be in the archive either")
	assert.NotContains(t, string(packed), gps,
		"the coordinates the photo was taken at must not leave the house")

	// Still a picture afterwards, so "clean" cannot be achieved by shipping
	// something unusable.
	_, err = jpeg.Decode(bytes.NewReader(packed))
	assert.NoError(t, err)

	// And the JSON points at the file that is actually there.
	var doc struct {
		Products []struct {
			ImageFile *string `json:"image_file"`
		} `json:"products"`
	}
	require.NoError(t, json.Unmarshal(files["export.json"], &doc))
	require.Len(t, doc.Products, 1)
	require.NotNil(t, doc.Products[0].ImageFile)
	assert.Equal(t, entry, *doc.Products[0].ImageFile)
}

// TestExportPacksOnlyThisStoragesOwnPictures — a product's image_url may name
// another storage's product-image route, or a provider URL from the suggestion
// flow. Neither is this storage's to hand out, and neither is a file in the
// permanent product area, so neither travels; the product simply exports with
// a null image_file.
func TestExportPacksOnlyThisStoragesOwnPictures(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	otherStorage := uuid.New()
	otherName := uuid.New().String() + ".jpg"
	require.NoError(t, f.pictures.Save(otherName, []byte("not ours")))
	foreign := productImageURLFor(otherStorage, otherName)
	provider := "https://images.example.test/butter.jpg"
	suggestion := "/api/storages/" + f.storageID.String() + "/images/" +
		strings.Repeat("a", 64)

	f.exports.data = &store.StorageExport{
		Storage: store.Storage{ID: f.storageID, Name: "Kitchen"},
		Products: []store.ExportProduct{
			{ID: uuid.New(), Name: "Another household's butter", ImageURL: &foreign},
			{ID: uuid.New(), Name: "Provider picture", ImageURL: &provider},
			{ID: uuid.New(), Name: "Suggestion cache picture", ImageURL: &suggestion},
		},
	}

	rec := f.do(http.MethodGet, f.base()+"/export", "")
	require.Equal(t, http.StatusOK, rec.Code)

	files := readArchive(t, rec.Body.Bytes())
	for name := range files {
		assert.False(t, strings.HasPrefix(name, "images/"),
			"no picture should have been packed, but the archive holds %q", name)
	}
	assert.NotContains(t, string(files["export.json"]), otherStorage.String(),
		"another storage's id must not reach the archive through an image path")
	assert.NotContains(t, string(files["export.json"]), provider,
		"an internal or provider path is not something an archive carries")

	var doc struct {
		Products []struct {
			ImageFile *string `json:"image_file"`
		} `json:"products"`
	}
	require.NoError(t, json.Unmarshal(files["export.json"], &doc))
	require.Len(t, doc.Products, 3)
	for i, p := range doc.Products {
		assert.Nil(t, p.ImageFile, "product %d must reference no file in this archive", i)
	}
}

// TestExportSkipsAPictureWhoseFileIsGone — a picture lost to a half-finished
// eviction must cost the member that one image, not their entire export.
func TestExportSkipsAPictureWhoseFileIsGone(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	missing := productImageURLFor(f.storageID, uuid.New().String()+".jpg")
	f.exports.data = &store.StorageExport{
		Storage: store.Storage{ID: f.storageID, Name: "Kitchen"},
		Products: []store.ExportProduct{
			{ID: uuid.New(), Name: "Butter", ImageURL: &missing, CurrentStock: 1},
		},
	}

	rec := f.do(http.MethodGet, f.base()+"/export", "")
	require.Equal(t, http.StatusOK, rec.Code, "one missing file must not fail the download")

	files := readArchive(t, rec.Body.Bytes())
	require.Contains(t, files, "export.json")

	var doc struct {
		Products []struct {
			Name      string  `json:"name"`
			ImageFile *string `json:"image_file"`
		} `json:"products"`
	}
	require.NoError(t, json.Unmarshal(files["export.json"], &doc))
	require.Len(t, doc.Products, 1)
	assert.Equal(t, "Butter", doc.Products[0].Name)
	assert.Nil(t, doc.Products[0].ImageFile,
		"the archive stays self-consistent: no reference to a file it does not hold")
}

// TestExportFailsBeforeWritingAnyBytes — once archive/zip has emitted a header
// the status line is gone, so a store error discovered mid-stream would become
// a 200 carrying a truncated ZIP: a corrupt file that looks like a successful
// download. The read happens first precisely so this is an ordinary error
// envelope instead.
func TestExportFailsBeforeWritingAnyBytes(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.exports.err = errors.New("connection reset by peer")

	rec := f.do(http.MethodGet, f.base()+"/export", "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "internal_error", errorCode(t, rec))
	assert.NotContains(t, rec.Header().Get("Content-Type"), "zip")
	assert.NotContains(t, rec.Body.String(), "connection reset",
		"the reason stays in the log; debug_reason is gated at the serializer")
}

// TestExportOfAnUnknownStorageIsTheStandard404 — the store's ErrNotFound goes
// through the same serializer as everything else, so a storage that vanished
// between the gate and the read is indistinguishable from one that never
// existed (docs/specs/03-auth-and-multi-tenancy.md).
func TestExportOfAnUnknownStorageIsTheStandard404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.exports.err = store.ErrNotFound

	rec := f.do(http.MethodGet, f.base()+"/export", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// TestExportIsRefusedToANonMemberWithThe404 states the criterion on its own
// rather than leaving it to the storageRoutes sweep: a non-member's export
// request gets the standard 404, byte for byte the same as for a storage id
// that names nothing, and the store is never asked.
func TestExportIsRefusedToANonMemberWithThe404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.removeMember(f.storageID, f.user.ID)

	member := f.do(http.MethodGet, f.base()+"/export", "")
	unknown := f.do(http.MethodGet, "/api/storages/"+uuid.New().String()+"/export", "")

	require.Equal(t, http.StatusNotFound, member.Code)
	require.Equal(t, http.StatusNotFound, unknown.Code)
	assert.Equal(t, unknown.Body.String(), member.Body.String(),
		"an inaccessible storage and an unknown one answer identically")
	assert.Zero(t, f.exports.callCount, "the gate refuses before the store is ever read")
}
