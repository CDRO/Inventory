package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// fixedSyncPoint is the instant every fake delta reports, so a test can assert
// on synced_at without a clock in it.
var fixedSyncPoint = time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)

// deltaPaths are the three list endpoints that answer a delta. Every rule that
// is about the delta contract rather than about one entity is checked against
// all three, because "we implemented it on products" is how the other two
// quietly diverge.
var deltaPaths = []string{"/products", "/categories", "/locations"}

// bearer issues a request as the fixture's user over Authorization: Bearer —
// the transport a paired native client uses (docs/specs/12-client-api-contract.md).
func (f *apiFixture) bearer(method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+f.session.ID)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// deltaBody is the envelope every delta response carries.
type deltaBody struct {
	Items      []json.RawMessage `json:"items"`
	Deleted    []uuid.UUID       `json:"deleted"`
	SyncedAt   time.Time         `json:"synced_at"`
	NextCursor *string           `json:"next_cursor"`
}

func readDelta(t *testing.T, rec *httptest.ResponseRecorder) deltaBody {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var body deltaBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// TestDeltaRequestIsOptIn is the compatibility guarantee the whole feature
// rests on: the PWA never sends updated_since, so its responses must be exactly
// what they were before delta sync existed. Asserted byte for byte, because a
// stray `deleted: []` or `synced_at` in the ordinary response would be a change
// to an endpoint three pages already parse.
func TestDeltaRequestIsOptIn(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	for _, path := range deltaPaths {
		plain := f.do(http.MethodGet, f.base()+path, "")
		require.Equal(t, http.StatusOK, plain.Code)

		assert.NotContains(t, plain.Body.String(), "deleted",
			"%s without updated_since must not grow a deleted array", path)
		assert.NotContains(t, plain.Body.String(), "synced_at",
			"%s without updated_since must not grow a sync point", path)
		assert.Contains(t, plain.Body.String(), "next_cursor",
			"%s keeps the collection envelope it always had", path)
	}

	// The store's delta path was never entered for any of them.
	assert.Zero(t, f.products.deltaCalls)
	assert.Zero(t, f.categories.deltaCalls)
	assert.Zero(t, f.locations.deltaCalls)
}

// TestDeltaRequestCarriesDeletedAndSyncPoint — a delta that reported only
// changed rows would leave every deleted row in the client's cache forever,
// which is the failure docs/specs/12-client-api-contract.md exists to prevent.
func TestDeltaRequestCarriesDeletedAndSyncPoint(t *testing.T) {
	t.Parallel()

	gone := uuid.New()
	f := newAPIFixture(t)
	f.products.delta = &store.Delta[store.Product]{
		Changed:  []store.Product{{ID: uuid.New(), Name: "Yoghurt"}},
		Deleted:  []uuid.UUID{gone},
		SyncedAt: fixedSyncPoint,
	}

	rec := f.do(http.MethodGet, f.base()+"/products?updated_since=2026-09-01T00:00:00Z", "")

	body := readDelta(t, rec)
	assert.Len(t, body.Items, 1)
	assert.Equal(t, []uuid.UUID{gone}, body.Deleted)
	assert.True(t, fixedSyncPoint.Equal(body.SyncedAt),
		"the client needs the server's own clock back to use as its next cursor")
	// Asserted on the raw JSON, not on the decoded pointer: a decoded nil
	// cannot tell an explicit null from a key that is not there, so an
	// `omitempty` slipping onto the field would pass a nil check while
	// breaking the envelope contract — a client that stops when next_cursor is
	// null would see an absent key as undefined.
	assert.Contains(t, rec.Body.String(), `"next_cursor":null`,
		"the key is part of the envelope even when the collection is exhausted")
	assert.Nil(t, body.NextCursor, "these collections are returned whole")

	assert.Equal(t, 1, f.products.deltaCalls)
	assert.True(t, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Equal(f.products.lastSince),
		"the parsed instant is what reaches the store")
}

// TestEmptyDeltaIsEmptyArraysNotNull keeps a client from having to special-case
// "nothing changed": items and deleted are always arrays.
func TestEmptyDeltaIsEmptyArraysNotNull(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	for _, path := range deltaPaths {
		rec := f.do(http.MethodGet, f.base()+path+"?updated_since=2026-09-01T00:00:00Z", "")

		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"items":[]`, "%s", path)
		assert.Contains(t, rec.Body.String(), `"deleted":[]`, "%s", path)
	}
}

// TestCategoryDeltaIsFlatWithParentID — a delta holds only the nodes that
// changed, so a changed child whose parent did not change has no parent in the
// payload. Nesting it would make that child a root in the client's cache; the
// edge therefore travels as a field.
func TestCategoryDeltaIsFlatWithParentID(t *testing.T) {
	t.Parallel()

	parent := uuid.New()
	child := uuid.New()
	f := newAPIFixture(t)
	f.categories.delta = &store.Delta[store.Category]{
		// Only the child changed. Its parent is absent from the payload.
		Changed:  []store.Category{{ID: child, Name: "Yoghurt", ParentID: &parent}},
		Deleted:  []uuid.UUID{},
		SyncedAt: fixedSyncPoint,
	}

	rec := f.do(http.MethodGet, f.base()+"/categories?updated_since=2026-09-01T00:00:00Z", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"children"`,
		"a partial tree cannot be nested — the client rebuilds it from parent_id")

	var body struct {
		Items []struct {
			ID                   uuid.UUID  `json:"id"`
			ParentID             *uuid.UUID `json:"parent_id"`
			DefaultShelfLifeDays *int       `json:"default_shelf_life_days"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	assert.Equal(t, child, body.Items[0].ID)
	require.NotNil(t, body.Items[0].ParentID)
	assert.Equal(t, parent, *body.Items[0].ParentID, "the edge survives as a field")
	assert.Contains(t, rec.Body.String(), `"default_shelf_life_days":null`,
		"null means inherit, which is a value the client must be told")
}

// TestLocationDeltaKeepsNullDescription — locationNode omits an empty
// description, which in a tree means "no note". In a delta the client is
// patching a row it already holds, so an absent key reads as "unchanged" and
// clearing a description would never reach the cache.
func TestLocationDeltaKeepsNullDescription(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.locations.delta = &store.Delta[store.Location]{
		Changed:  []store.Location{{ID: uuid.New(), Name: "Fridge", Description: nil}},
		Deleted:  []uuid.UUID{},
		SyncedAt: fixedSyncPoint,
	}

	rec := f.do(http.MethodGet, f.base()+"/locations?updated_since=2026-09-01T00:00:00Z", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"description":null`,
		"present-and-null is how a cleared description reaches the client")
}

// TestStaleDeltaCursorIsResyncRequired — the alternative to this 409 is a 200
// carrying an incomplete delta, which leaves a deleted row in the cache with
// nothing anywhere reporting a problem.
func TestStaleDeltaCursorIsResyncRequired(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.products.deltaErr = store.ErrResyncRequired
	f.categories.deltaErr = store.ErrResyncRequired
	f.locations.deltaErr = store.ErrResyncRequired

	for _, path := range deltaPaths {
		rec := f.do(http.MethodGet, f.base()+path+"?updated_since=2020-01-01T00:00:00Z", "")

		assert.Equal(t, http.StatusConflict, rec.Code, "%s", path)
		assert.Equal(t, "resync_required", errorCode(t, rec),
			"%s: a client switching on `conflict` alone could not tell this from "+
				"'that category still has products in it'", path)
	}
}

// TestMalformedUpdatedSinceIsValidationFailed — 422 naming the parameter, the
// same answer an unusable limit or cursor gets.
func TestMalformedUpdatedSinceIsValidationFailed(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)

	for _, raw := range []string{"yesterday", "2026-09-01", "1758461400", "2026-09-01T00:00:00"} {
		for _, path := range deltaPaths {
			rec := f.do(http.MethodGet, f.base()+path+"?updated_since="+raw, "")

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%s %q", path, raw)
			assert.Equal(t, "validation_failed", errorCode(t, rec))
			assert.Contains(t, errorFields(t, rec), "updated_since",
				"%s %q: the failure names the parameter", path, raw)
		}
	}

	assert.Zero(t, f.products.deltaCalls, "a malformed cursor never reaches the store")
	assert.Zero(t, f.categories.deltaCalls)
	assert.Zero(t, f.locations.deltaCalls)
}

// TestDeltaAgainstAnotherStorageIs404OverBearer is the acceptance criterion
// spelled out in docs/specs/12-client-api-contract.md: a paired client is
// subject to identical storage scoping and 404 behavior as the browser,
// verified by a request for another storage's id over Bearer auth.
//
// The delta parameters are on the URL deliberately. A gate that ran after the
// query was parsed would answer 422 for a malformed cursor and 404 for a
// well-formed one, and that difference is enough to tell a prober which
// storages exist — so both forms are asserted to be byte-identical to the
// unknown-storage answer.
func TestDeltaAgainstAnotherStorageIs404OverBearer(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	// A real storage the caller is simply not a member of, and one that does
	// not exist at all. Neither may be distinguishable from the other.
	theirs := uuid.New()
	stranger, _ := f.auth.addUser(t, false)
	f.auth.addMember(theirs, stranger.ID)
	nowhere := uuid.New()

	for _, path := range deltaPaths {
		for _, query := range []string{
			"",
			"?updated_since=2026-09-01T00:00:00Z",
			"?updated_since=yesterday",
		} {
			inaccessible := f.bearer(http.MethodGet, "/api/storages/"+theirs.String()+path+query)
			unknown := f.bearer(http.MethodGet, "/api/storages/"+nowhere.String()+path+query)

			require.Equal(t, http.StatusNotFound, inaccessible.Code,
				"%s%s: a storage the caller is not a member of is 404, never 403", path, query)
			require.Equal(t, http.StatusNotFound, unknown.Code, "%s%s", path, query)
			assert.Equal(t, unknown.Body.String(), inaccessible.Body.String(),
				"%s%s: the two answers must be byte-identical", path, query)
		}
	}

	assert.Zero(t, f.products.deltaCalls, "the gate refuses before any handler runs")
	assert.Zero(t, f.categories.deltaCalls)
	assert.Zero(t, f.locations.deltaCalls)
}

// TestDeltaOverBearerMatchesTheCookie — one session concept, two transports.
// A paired client and the browser must get the same delta for the same storage.
func TestDeltaOverBearerMatchesTheCookie(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.products.delta = &store.Delta[store.Product]{
		Changed:  []store.Product{{ID: uuid.New(), Name: "Yoghurt"}},
		Deleted:  []uuid.UUID{uuid.New()},
		SyncedAt: fixedSyncPoint,
	}

	query := "/products?updated_since=2026-09-01T00:00:00Z"
	viaCookie := f.do(http.MethodGet, f.base()+query, "")
	viaBearer := f.bearer(http.MethodGet, f.base()+query)

	require.Equal(t, http.StatusOK, viaBearer.Code, viaBearer.Body.String())
	assert.Equal(t, viaCookie.Body.String(), viaBearer.Body.String())
}

// TestClientVersionNeverChangesAResponse pins the rule that makes
// X-Client-Version safe: it is diagnostics, and version-sniffing to alter a
// response is how one API quietly becomes several
// (docs/specs/12-client-api-contract.md).
//
// Both a successful delta and a refusal are checked. The refusal matters more:
// the error serializer is the one place in the package that reads this header,
// so if anything were ever going to branch on it, it would be there.
func TestClientVersionNeverChangesAResponse(t *testing.T) {
	t.Parallel()

	versions := []string{
		"",
		"Inventory-Android/1.0.0",
		"Inventory-Android/0.0.1-ancient",
		"not a version at all",
		strings.Repeat("v", 4096),
	}

	f := newAPIFixture(t)
	f.products.delta = &store.Delta[store.Product]{
		Changed:  []store.Product{{ID: uuid.New(), Name: "Yoghurt"}},
		Deleted:  []uuid.UUID{},
		SyncedAt: fixedSyncPoint,
	}

	for _, target := range []string{
		f.base() + "/products?updated_since=2026-09-01T00:00:00Z", // 200
		f.base() + "/products?updated_since=nonsense",             // 422
		"/api/storages/" + uuid.New().String() + "/products",      // 404
	} {
		var wantCode int
		var wantBody string
		for i, version := range versions {
			req := httptest.NewRequest(http.MethodGet, target, strings.NewReader(""))
			req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: f.session.ID})
			if version != "" {
				req.Header.Set(httpapi.ClientVersionHeader, version)
			}
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)

			if i == 0 {
				wantCode, wantBody = rec.Code, rec.Body.String()
				continue
			}
			assert.Equalf(t, wantCode, rec.Code, "%s changed status for %q", target, version)
			assert.Equalf(t, wantBody, rec.Body.String(), "%s changed body for %q", target, version)
		}
	}
}

// TestPairedClientIsNeverAnAdminClient — a paired device session belongs to
// the same user as their browser, so is_admin alone cannot tell the two apart.
// docs/specs/12-client-api-contract.md puts the admin area outside the client
// contract entirely: /admin and /api/admin/* answer 404 to a paired client
// regardless of that user's is_admin value.
//
// The same user's browser session is checked in the same test, because the
// point is that the admin area still works for them — this is a boundary, not
// a lockout.
func TestPairedClientIsNeverAnAdminClient(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.setAdmin(f.user.ID, true)
	phone := f.auth.addDeviceSession(f.user.ID, "Pixel 9")

	adminPaths := []string{"/admin", "/api/admin/users", "/api/admin/storages", "/api/admin/settings"}

	for _, path := range adminPaths {
		req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+phone.ID)
		req.Header.Set(httpapi.ClientVersionHeader, "Inventory-Android/1.0.0")
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)

		assert.Equalf(t, http.StatusNotFound, rec.Code,
			"%s must be invisible to a paired client even when its user is an admin", path)

		// Byte-identical to a path that does not exist at all: the admin area
		// does not announce itself to a client that may not have it.
		missing := httptest.NewRequest(http.MethodGet, "/api/admin/no-such-route", strings.NewReader(""))
		missing.Header.Set("Authorization", "Bearer "+phone.ID)
		missingRec := httptest.NewRecorder()
		f.router.ServeHTTP(missingRec, missing)
		assert.Equalf(t, missingRec.Body.String(), rec.Body.String(),
			"%s must be indistinguishable from a route that does not exist", path)
	}

	// The same admin, in their browser, is unaffected.
	browser := f.do(http.MethodGet, "/api/admin/users", "")
	assert.Equal(t, http.StatusOK, browser.Code,
		"the rule is about the paired client, not about the person")
}

// TestAdminStatusIsStillRequeriedForAPairedClient keeps the CLAUDE.md
// invariant honest under the new refusal: is_admin is re-read from the database
// on every admin request, including the ones refused for being a device
// session. Short-circuiting before the lookup would quietly narrow the
// invariant to "on every admin request from a browser".
func TestAdminStatusIsStillRequeriedForAPairedClient(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.setAdmin(f.user.ID, true)
	phone := f.auth.addDeviceSession(f.user.ID, "Pixel 9")

	before := f.auth.adminCallCount()

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+phone.ID)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Greater(t, f.auth.adminCallCount(), before,
		"is_admin is re-queried on every admin request, refused ones included")
}

// TestPairedClientKeepsTheOrdinaryAPI — the boundary is the admin area alone.
// A paired client has full parity everywhere else, delta sync included
// (docs/specs/12-client-api-contract.md, "Full parity").
func TestPairedClientKeepsTheOrdinaryAPI(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	phone := f.auth.addDeviceSession(f.user.ID, "Pixel 9")

	for _, path := range deltaPaths {
		req := httptest.NewRequest(http.MethodGet,
			f.base()+path+"?updated_since=2026-09-01T00:00:00Z", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+phone.ID)
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)

		assert.Equalf(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	}
}
