package httpapi_test

import (
	"bytes"
	"image"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/derive"
	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/uploads"
)

// withVariants gives the fixture a real derive service on a temporary cache,
// so the variant routes exercise the lazy path end to end.
func withVariants(t *testing.T) func(*httpapi.Deps) {
	t.Helper()
	derived, err := uploads.NewDerived(filepath.Join(t.TempDir(), "derived"))
	require.NoError(t, err)
	return func(d *httpapi.Deps) { d.Variants = derive.New(derived, nil) }
}

func pictureSize(t *testing.T, body []byte) [2]int {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	require.NoError(t, err)
	return [2]int{cfg.Width, cfg.Height}
}

func TestJobImageVariantsServeThePhotoAtScreenSizes(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t, withVariants(t))
	job := f.reviewJob(t)
	base := f.base() + "/jobs/" + job.ID.String() + "/image"

	res := f.do(http.MethodGet, base+"/thumb-96", "")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Equal(t, "image/jpeg", res.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", res.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, res.Header().Get("Cache-Control"), "private")
	assert.Equal(t, [2]int{20, 20}, pictureSize(t, res.Body.Bytes()), "the 40×20 photo's centred square, never scaled up")

	res = f.do(http.MethodGet, base+"/preview", "")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, [2]int{40, 20}, pictureSize(t, res.Body.Bytes()))

	for _, bad := range []string{"/thumb-100", "/original", "/Preview", "/rows"} {
		res = f.do(http.MethodGet, base+bad, "")
		assert.Equal(t, http.StatusNotFound, res.Code, bad)
	}

	// The bare route still serves the original, as the API contract promises.
	res = f.do(http.MethodGet, base, "")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, "image/png", res.Header().Get("Content-Type"))
	assert.Equal(t, quadrantPNG(t), res.Body.Bytes())
}

func TestJobRowCropsFollowTheProposal(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t, withVariants(t))
	job := f.reviewJob(t)
	base := f.base() + "/jobs/" + job.ID.String() + "/image/rows/"

	res := f.do(http.MethodGet, base+"0/thumb-192", "")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Equal(t, "image/jpeg", res.Header().Get("Content-Type"))
	assert.Equal(t, [2]int{10, 10}, pictureSize(t, res.Body.Bytes()), "the bottom-right quadrant is 20×10; its centred square is 10")
	img, _, err := image.Decode(bytes.NewReader(res.Body.Bytes()))
	require.NoError(t, err)
	r, g, b, _ := img.At(5, 5).RGBA()
	assert.Greater(t, r>>8, uint32(200), "the crop is the yellow quadrant")
	assert.Greater(t, g>>8, uint32(200))
	assert.Less(t, b>>8, uint32(80))

	res = f.do(http.MethodGet, base+"0/thumb-384", "")
	assert.Equal(t, http.StatusOK, res.Code)

	for _, bad := range []string{"1/thumb-192", "7/thumb-192", "0/thumb-96", "0/preview", "x/thumb-192"} {
		res = f.do(http.MethodGet, base+bad, "")
		assert.Equal(t, http.StatusNotFound, res.Code, bad)
	}
}

func TestJobImageVariantsNeedAJobWithAPhoto(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t, withVariants(t))

	res := f.do(http.MethodGet, f.base()+"/jobs/"+uuid.NewString()+"/image/thumb-96", "")
	assert.Equal(t, http.StatusNotFound, res.Code, "no such job")

	bare := f.jobs.add(t, f.storageID, store.JobDone, `{"rows":[]}`)
	res = f.do(http.MethodGet, f.base()+"/jobs/"+bare.ID.String()+"/image/thumb-96", "")
	assert.Equal(t, http.StatusNotFound, res.Code, "a job without a photo")
	res = f.do(http.MethodGet, f.base()+"/jobs/"+bare.ID.String()+"/image/rows/0/thumb-192", "")
	assert.Equal(t, http.StatusNotFound, res.Code)

	// Without a cache volume the variant routes are 404 and the original still serves.
	plain := newAPIFixture(t)
	job := plain.reviewJob(t)
	res = plain.do(http.MethodGet, plain.base()+"/jobs/"+job.ID.String()+"/image/thumb-96", "")
	assert.Equal(t, http.StatusNotFound, res.Code)
	res = plain.do(http.MethodGet, plain.base()+"/jobs/"+job.ID.String()+"/image", "")
	assert.Equal(t, http.StatusOK, res.Code)
}

func TestProductPictureVariantsServeStoredPicturesAtScreenSizes(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t, withVariants(t))
	name := uuid.Must(uuid.NewV7()).String() + ".png"
	f.pictures.files[name] = quadrantPNG(t)
	url := f.base() + "/product-images/" + name
	f.ingest.markImageUsed(f.storageID, url)

	res := f.do(http.MethodGet, url+"/thumb-96", "")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Equal(t, "image/jpeg", res.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", res.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, res.Header().Get("Cache-Control"), "private")
	assert.Equal(t, [2]int{20, 20}, pictureSize(t, res.Body.Bytes()))

	res = f.do(http.MethodGet, url+"/thumb-768", "")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, [2]int{20, 20}, pictureSize(t, res.Body.Bytes()), "never scaled up")

	res = f.do(http.MethodGet, url+"/preview", "")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, [2]int{40, 20}, pictureSize(t, res.Body.Bytes()))

	res = f.do(http.MethodGet, url+"/thumb-12", "")
	assert.Equal(t, http.StatusNotFound, res.Code)

	// The original route is unchanged: the bytes as stored.
	res = f.do(http.MethodGet, url, "")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, quadrantPNG(t), res.Body.Bytes())

	// A picture no product in this storage uses is 404 at every size — the
	// same answer as for one that does not exist, before any file is read.
	other := uuid.Must(uuid.NewV7()).String() + ".png"
	f.pictures.files[other] = quadrantPNG(t)
	res = f.do(http.MethodGet, f.base()+"/product-images/"+other+"/thumb-96", "")
	assert.Equal(t, http.StatusNotFound, res.Code)
	res = f.do(http.MethodGet, f.base()+"/product-images/"+uuid.NewString()+".png/thumb-96", "")
	assert.Equal(t, http.StatusNotFound, res.Code)
}

func TestProductPictureVariantsServeAnIconAsItIs(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t, withVariants(t))
	name := uuid.Must(uuid.NewV7()).String() + ".svg"
	icon := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path d="M3 3h18v18H3z"/></svg>`)
	f.pictures.files[name] = icon
	url := f.base() + "/product-images/" + name
	f.ingest.markImageUsed(f.storageID, url)

	for _, variant := range []string{"/thumb-96", "/thumb-768", "/preview"} {
		res := f.do(http.MethodGet, url+variant, "")
		require.Equal(t, http.StatusOK, res.Code, variant)
		assert.Equal(t, "image/svg+xml", res.Header().Get("Content-Type"))
		assert.Equal(t, "default-src 'none'", res.Header().Get("Content-Security-Policy"))
		assert.Equal(t, icon, res.Body.Bytes())
	}
}

// TestProductPictureVariantsWithoutACacheServeTheStoredPicture — a
// deployment whose cache volume is unusable still shows pictures: every size
// answers with the picture as stored, which is what every size got before.
func TestProductPictureVariantsWithoutACacheServeTheStoredPicture(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	name := uuid.Must(uuid.NewV7()).String() + ".png"
	f.pictures.files[name] = quadrantPNG(t)
	url := f.base() + "/product-images/" + name
	f.ingest.markImageUsed(f.storageID, url)

	res := f.do(http.MethodGet, url+"/thumb-96", "")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, "image/png", res.Header().Get("Content-Type"))
	assert.Equal(t, quadrantPNG(t), res.Body.Bytes())
}
