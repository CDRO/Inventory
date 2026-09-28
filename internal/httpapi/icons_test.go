package httpapi_test

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// fakeIcons is an in-memory IconStore (docs/specs/42-local-icon-library.md)
// and IconSuggestionStore (docs/specs/40-icon-picker.md) — the same fake
// covers both, since a real Store implements both against the same tables.
type fakeIcons struct {
	createErr  error
	lastCreate store.NewIcon
	created    []store.Icon

	svgs   map[uuid.UUID]string
	svgErr error

	aliasHits    []store.IconSuggestion
	aliasErr     error
	nameHits     []store.IconSuggestion
	nameErr      error
	createAlias  error
	lastAliasFor string
	lastAlias    string
	lastAliasBy  uuid.UUID
}

func (f *fakeIcons) CreateIcon(_ context.Context, in store.NewIcon) (*store.Icon, error) {
	f.lastCreate = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	createdBy := in.CreatedBy
	icon := store.Icon{ID: uuid.New(), Name: in.Name, SVGBody: in.SVGBody, Source: store.IconSourceUploaded, CreatedBy: &createdBy}
	f.created = append(f.created, icon)
	if f.svgs == nil {
		f.svgs = map[uuid.UUID]string{}
	}
	f.svgs[icon.ID] = icon.SVGBody
	return &icon, nil
}

func (f *fakeIcons) IconSVG(_ context.Context, id uuid.UUID) (string, error) {
	if f.svgErr != nil {
		return "", f.svgErr
	}
	body, ok := f.svgs[id]
	if !ok {
		return "", store.ErrNotFound
	}
	return body, nil
}

func (f *fakeIcons) SearchIconAliases(_ context.Context, _ string, _ int) ([]store.IconSuggestion, error) {
	return f.aliasHits, f.aliasErr
}

func (f *fakeIcons) SearchIconsByName(_ context.Context, _ string, _ int) ([]store.IconSuggestion, error) {
	return f.nameHits, f.nameErr
}

func (f *fakeIcons) CreateIconAlias(_ context.Context, iconName, alias string, createdBy uuid.UUID) error {
	f.lastAliasFor, f.lastAlias, f.lastAliasBy = iconName, alias, createdBy
	return f.createAlias
}

// doMultipart posts a multipart/form-data body carrying fields, and a file
// part named fileField when fileField is not empty, with the fixture's
// session cookie.
func doMultipart(t *testing.T, f *apiFixture, path string, fields map[string]string, fileField, fileName string, fileContent []byte) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, writer.WriteField(k, v))
	}
	if fileField != "" {
		part, err := writer.CreateFormFile(fileField, fileName)
		require.NoError(t, err)
		_, err = part.Write(fileContent)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: f.session.ID})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

const validSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`

func TestUploadIconRequiresNameAndSVG(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := doMultipart(t, f, f.base()+"/icons", map[string]string{}, "", "", nil)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	fields := errorFields(t, rec)
	assert.Contains(t, fields, "name")
	assert.Contains(t, fields, "svg")
}

func TestUploadIconRejectsMalformedSVG(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := doMultipart(t, f, f.base()+"/icons",
		map[string]string{"name": "custom:broken", "svg": "<svg><rect></svg>"}, "", "", nil)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, errorFields(t, rec), "svg")
}

// TestUploadIconRejectsContentAfterTheRootElement is the regression for a
// round-1 go review finding: xml.Unmarshal into a struct that only names
// XMLName stops as soon as the root element is filled and never notices
// anything after it, so a payload smuggling a second top-level element past
// a well-formed <svg>...</svg> used to pass isWellFormedSVG and be stored
// verbatim.
func TestUploadIconRejectsContentAfterTheRootElement(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := doMultipart(t, f, f.base()+"/icons",
		map[string]string{"name": "custom:smuggled", "svg": validSVG + "<script>alert(1)</script>"}, "", "", nil)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, errorFields(t, rec), "svg")
}

func TestUploadIconRejectsNonSVGXML(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := doMultipart(t, f, f.base()+"/icons",
		map[string]string{"name": "custom:not-svg", "svg": "<html><body>hi</body></html>"}, "", "", nil)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, errorFields(t, rec), "svg")
}

func TestUploadIconRejectsNameOver100Chars(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	long := ""
	for i := 0; i < 101; i++ {
		long += "a"
	}
	rec := doMultipart(t, f, f.base()+"/icons",
		map[string]string{"name": long, "svg": validSVG}, "", "", nil)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, errorFields(t, rec), "name")
}

func TestUploadIconAcceptsAPastedSVGStringField(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := doMultipart(t, f, f.base()+"/icons",
		map[string]string{"name": "  custom:pasted  ", "svg": validSVG}, "", "", nil)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "custom:pasted", f.icons.lastCreate.Name, "the name is trimmed before it is stored")
	assert.Equal(t, validSVG, f.icons.lastCreate.SVGBody)
	assert.Equal(t, f.user.ID, f.icons.lastCreate.CreatedBy)

	assert.Contains(t, rec.Body.String(), `"source":"uploaded"`)
	assert.NotContains(t, rec.Body.String(), "svg_body", "the body is fetched separately, through ServeSVG")
}

func TestUploadIconAcceptsAFilePart(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := doMultipart(t, f, f.base()+"/icons",
		map[string]string{"name": "custom:from-file"}, "svg", "icon.svg", []byte(validSVG))

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, validSVG, f.icons.lastCreate.SVGBody)
}

func TestUploadIconNameCollisionIs422(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.icons.createErr = fmt.Errorf("%w: an icon named %q already exists", store.ErrValidation, "noto:cheese-wedge")

	rec := doMultipart(t, f, f.base()+"/icons",
		map[string]string{"name": "noto:cheese-wedge", "svg": validSVG}, "", "", nil)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, "validation_failed", errorCode(t, rec))
}

func TestServeIconSVGSetsRenderingHeaders(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	id := uuid.New()
	f.icons.svgs = map[uuid.UUID]string{id: validSVG}

	rec := f.do(http.MethodGet, f.base()+"/icons/"+id.String()+"/svg", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "default-src 'none'", rec.Header().Get("Content-Security-Policy"),
		"the CSP is what makes an uploaded SVG safe to serve without a content sanitizer")
	assert.Equal(t, validSVG, rec.Body.String())
}

func TestServeIconSVGUnknownIDIs404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, f.base()+"/icons/"+uuid.New().String()+"/svg", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}
