package httpapi_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// quadrantPNG is a 40×20 PNG whose bottom-right quadrant is yellow and whose
// other three are not, so a crop can be told apart from the whole photo by
// size and by colour.
func quadrantPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			c := color.RGBA{R: 30, G: 30, B: 30, A: 255}
			if x >= 20 && y >= 10 {
				c = color.RGBA{R: 255, G: 255, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// reviewJob seeds a done shelf job in the fixture's storage whose photo is
// quadrantPNG. Row "0" has a box over the yellow quadrant; row "1" has none.
func (f *apiFixture) reviewJob(t *testing.T) *store.Job {
	t.Helper()
	name := uuid.Must(uuid.NewV7()).String() + ".png"
	f.photos.files[name] = quadrantPNG(t)

	job := f.jobs.add(t, f.storageID, store.JobDone, `{"rows":[
	  {"row_id":"0","bounding_box":{"x":0.5,"y":0.5,"width":0.5,"height":0.5}},
	  {"row_id":"1","bounding_box":null}
	]}`)
	job.ImageFilename = &name
	return job
}

func confirmPath(f *apiFixture, job *store.Job) string {
	return f.base() + "/ingest/" + job.ID.String() + "/confirm"
}

// newProductRow is an accepted row creating a product named name, with image
// as the raw JSON value of new_product.image ("" to leave it out).
func newProductRow(rowID, name, image string) string {
	img := ""
	if image != "" {
		img = `,"image":` + image
	}
	return `{"row_id":"` + rowID + `","decision":"accept","new_product":{"name":"` + name + `"` + img + `},
	  "quantity":1,"location_id":"` + uuid.NewString() + `"}`
}

func decodedSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	return cfg.Width, cfg.Height
}

func TestConfirmTakesANewProductsPictureFromTheReviewedPhoto(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := f.reviewJob(t)

	rec := f.do(http.MethodPost, confirmPath(f, job), `{"items":[`+
		newProductRow("0", "Chutney", `"crop"`)+`,`+
		newProductRow("1", "Rolled Oats", `"photo"`)+`]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	d := f.ingest.decisions
	require.Len(t, d, 2)
	prefix := "/api/storages/" + f.storageID.String() + "/product-images/"

	crop, whole := d[0].NewProduct.ImageURL, d[1].NewProduct.ImageURL
	require.NotNil(t, crop, "the crop row's product gets a picture")
	require.NotNil(t, whole, "the whole-photo row's product gets a picture")
	assert.True(t, strings.HasPrefix(*crop, prefix), *crop)
	assert.True(t, strings.HasPrefix(*whole, prefix), *whole)
	assert.NotEqual(t, *crop, *whole)

	cropData := f.pictures.files[strings.TrimPrefix(*crop, prefix)]
	wholeData := f.pictures.files[strings.TrimPrefix(*whole, prefix)]
	require.NotEmpty(t, cropData, "the crop is written to permanent storage")
	require.NotEmpty(t, wholeData, "the whole photo is written to permanent storage")

	w, h := decodedSize(t, cropData)
	assert.Equal(t, [2]int{20, 10}, [2]int{w, h}, "the crop is the row's own box, not the whole photo")
	w, h = decodedSize(t, wholeData)
	assert.Equal(t, [2]int{40, 20}, [2]int{w, h})

	// And the stored picture is what the URL recorded on the product serves.
	served := f.do(http.MethodGet, *crop, "")
	require.Equal(t, http.StatusOK, served.Code)
	assert.Equal(t, "image/png", served.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", served.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, served.Header().Get("Cache-Control"), "private")
	assert.Equal(t, cropData, served.Body.Bytes())
}

func TestConfirmWithoutAPictureWritesNone(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := f.reviewJob(t)

	for _, image := range []string{"", "null"} {
		rec := f.do(http.MethodPost, confirmPath(f, job), `{"items":[`+newProductRow("0", "Chutney", image)+`,{"row_id":"1","decision":"reject"}]}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Nil(t, f.ingest.decisions[0].NewProduct.ImageURL)
	}
	assert.Empty(t, f.pictures.files)
}

func TestConfirmRefusesAPictureItCannotTake(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		setup func(f *apiFixture, job *store.Job)
		rows  string
	}{
		"an unknown source": {
			rows: newProductRow("0", "Chutney", `"selfie"`),
		},
		"a crop of a row with no box": {
			rows: newProductRow("1", "Chutney", `"crop"`),
		},
		"a crop whose box selects nothing": {
			setup: func(f *apiFixture, job *store.Job) {
				job.Payload = []byte(`{"rows":[{"row_id":"0","bounding_box":{"x":2,"y":2,"width":0.1,"height":0.1}}]}`)
			},
			rows: newProductRow("0", "Chutney", `"crop"`),
		},
		"a job with no photo": {
			setup: func(_ *apiFixture, job *store.Job) { job.ImageFilename = nil },
			rows:  newProductRow("0", "Chutney", `"photo"`),
		},
		"a photo already swept from disk": {
			setup: func(f *apiFixture, job *store.Job) { delete(f.photos.files, *job.ImageFilename) },
			rows:  newProductRow("0", "Chutney", `"photo"`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			job := f.reviewJob(t)
			if tc.setup != nil {
				tc.setup(f, job)
			}

			rec := f.do(http.MethodPost, confirmPath(f, job), `{"items":[`+tc.rows+`]}`)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), `"items[0].new_product.image"`)
			assert.Nil(t, f.ingest.decisions, "nothing reaches the store")
			assert.Empty(t, f.pictures.files, "no picture is left behind")
		})
	}
}

func TestConfirmThatFailsLeavesNoPictureBehind(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := f.reviewJob(t)
	// A double submit: the job was consumed between the picture being written
	// and the transaction running.
	f.ingest.err = store.ErrConflict

	rec := f.do(http.MethodPost, confirmPath(f, job), `{"items":[`+newProductRow("0", "Chutney", `"crop"`)+`]}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Empty(t, f.pictures.files)
}

func TestConfirmRemovesAPictureNoProductEndedUpUsing(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := f.reviewJob(t)

	// Both rows name the same new product, so the store creates it once, with
	// the first row's picture. The second row's picture has no product.
	rec := f.do(http.MethodPost, confirmPath(f, job), `{"items":[`+
		newProductRow("0", "Chutney", `"crop"`)+`,`+
		newProductRow("1", "chutney", `"photo"`)+`]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Len(t, f.pictures.files, 1, "only the picture a product uses is kept")
	kept := *f.ingest.decisions[0].NewProduct.ImageURL
	assert.True(t, strings.HasSuffix(kept, "/"+onlyKey(f.pictures.files)))
}

func onlyKey(m map[string][]byte) string {
	for k := range m {
		return k
	}
	return ""
}

func TestConfirmOfAJobNotReadyCutsNoPicture(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := f.reviewJob(t)
	job.Status = store.JobConsumed
	f.ingest.err = store.ErrConflict

	rec := f.do(http.MethodPost, confirmPath(f, job), `{"items":[`+newProductRow("0", "Chutney", `"crop"`)+`]}`)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Empty(t, f.pictures.files)
}

func TestConfirmNamingAnotherStoragesJobIsANotFound(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	foreign := f.jobs.add(t, uuid.New(), store.JobDone, `{"rows":[{"row_id":"0","bounding_box":null}]}`)

	rec := f.do(http.MethodPost, confirmPath(f, foreign), `{"items":[`+newProductRow("0", "Chutney", `"photo"`)+`]}`)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Empty(t, f.pictures.files)
}

// TestProductPictureIsServedOnlyInTheStorageUsingIt — a stored picture is a
// photo from inside someone's home. Every way of asking for one that is not
// "a member of the storage whose product uses it" is the same 404.
func TestProductPictureIsServedOnlyInTheStorageUsingIt(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	base := f.base() + "/product-images/"

	mine := uuid.Must(uuid.NewV7()).String() + ".jpg"
	f.pictures.files[mine] = []byte("\xFF\xD8\xFFmine")
	f.ingest.markImageUsed(f.storageID, base+mine)

	// On disk, and used by a product — but in another storage.
	theirs := uuid.Must(uuid.NewV7()).String() + ".jpg"
	f.pictures.files[theirs] = []byte("\xFF\xD8\xFFtheirs")
	other := uuid.New()
	f.ingest.markImageUsed(other, "/api/storages/"+other.String()+"/product-images/"+theirs)

	// On disk, used by nothing: what a crash between write and commit leaves.
	orphan := uuid.Must(uuid.NewV7()).String() + ".jpg"
	f.pictures.files[orphan] = []byte("\xFF\xD8\xFForphan")

	// Used by a product, but the file is gone.
	missing := uuid.Must(uuid.NewV7()).String() + ".jpg"
	f.ingest.markImageUsed(f.storageID, base+missing)

	ok := f.do(http.MethodGet, base+mine, "")
	require.Equal(t, http.StatusOK, ok.Code)
	assert.Equal(t, "\xFF\xD8\xFFmine", ok.Body.String())
	assert.Equal(t, "image/jpeg", ok.Header().Get("Content-Type"))

	reference := f.do(http.MethodGet, base+uuid.Must(uuid.NewV7()).String()+".jpg", "")
	require.Equal(t, http.StatusNotFound, reference.Code)

	for label, name := range map[string]string{
		"another storage's picture":         theirs,
		"a picture no product uses":         orphan,
		"a used picture not on disk":        missing,
		"a name the server never generates": "..%2F..%2Fetc%2Fpasswd",
	} {
		rec := f.do(http.MethodGet, base+name, "")
		assert.Equal(t, http.StatusNotFound, rec.Code, label)
		assert.Equal(t, reference.Body.String(), rec.Body.String(), label+": indistinguishable from a picture that never existed")
		assert.NotContains(t, rec.Body.String(), "theirs", label)
	}
}
