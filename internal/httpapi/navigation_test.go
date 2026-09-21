package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// navFixture is an apiFixture plus the users GET /no-storages exists to sort
// between. The fixture's own user is a member of a storage, which is one of
// the four cases; the three added here are the rest.
type navFixture struct {
	*apiFixture

	adminNoStorage    *store.User
	adminNoStorageSes *store.Session
	plainNoStorageSes *store.Session
	adminWithSes      *store.Session
}

func newNavFixture(t *testing.T, opts ...func(*httpapi.Deps)) *navFixture {
	t.Helper()

	f := newAPIFixture(t, opts...)

	adminNoStorage, adminNoStorageSes := f.auth.addUser(t, true)
	_, plainNoStorageSes := f.auth.addUser(t, false)
	adminWithStorage, adminWithSes := f.auth.addUser(t, true)
	f.auth.addMember(f.storageID, adminWithStorage.ID)

	return &navFixture{
		apiFixture:        f,
		adminNoStorage:    adminNoStorage,
		adminNoStorageSes: adminNoStorageSes,
		plainNoStorageSes: plainNoStorageSes,
		adminWithSes:      adminWithSes,
	}
}

// TestNoStoragesRedirectsByExactLocation walks the whole table in
// docs/specs/29-first-run-admin-guidance.md.
//
// Every case asserts the **exact** Location. "A redirect happened" is the one
// assertion this route must not be tested with: all four answers are 302s, so
// a test satisfied by the status code alone would pass with every caller sent
// to the same place — including every non-admin sent to the admin area, which
// is the failure this design exists to make impossible.
func TestNoStoragesRedirectsByExactLocation(t *testing.T) {
	t.Parallel()

	f := newNavFixture(t)

	cases := []struct {
		name     string
		session  string
		location string
	}{
		{
			// No session at all: a redirect, not the 401 envelope the API
			// answers with. This is a route a browser navigates to, and a
			// page of JSON in the address bar is not an answer to a person.
			name:     "no session goes to the login page",
			session:  "",
			location: "/index.html",
		},
		{
			// The other way to have no session, and the one a real person
			// actually meets: a cookie that is still in the browser naming a
			// row that is gone — logged out elsewhere, revoked from the
			// devices list, or simply expired. It reaches the gate through a
			// different branch than the case above (LookupSession's
			// ErrNotFound rather than an empty id), and it must come out at
			// the same place.
			name:     "an expired or revoked session goes to the login page too",
			session:  "sess-no-such-row",
			location: "/index.html",
		},
		{
			name:     "a member goes to the storages page",
			session:  f.session.ID,
			location: "/storages.html",
		},
		{
			// The boundary the spec calls out: an admin is redirected away
			// from the storages page only while they have nothing there. Once
			// they have added themselves to a storage they use the app like
			// anyone else, and a redirect to the admin area here would trap
			// them out of it.
			name:     "an admin who is a member goes to the storages page like anyone else",
			session:  f.adminWithSes.ID,
			location: "/storages.html",
		},
		{
			name:     "an admin with no storage goes to the admin area",
			session:  f.adminNoStorageSes.ID,
			location: "/admin",
		},
		{
			// The case that proves the route is not behind RequireAdmin: that
			// gate answers 404 to a non-admin, which would strand exactly the
			// users this route exists to send somewhere useful.
			name:     "a non-admin with no storage goes to the empty state",
			session:  f.plainNoStorageSes.ID,
			location: "/storages.html?empty=1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := sendAs(f.router, tc.session, http.MethodGet, "/no-storages", "")

			require.Equal(t, http.StatusFound, rec.Code)
			assert.Equal(t, tc.location, rec.Header().Get("Location"))
			// Every answer, in every state. A navigation decision held in a
			// browser's cache keeps sending a user where they belonged one
			// membership change ago.
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			assert.Empty(t, rec.Result().Cookies(), "the route sets no cookie in any state")
		})
	}
}

// TestNoStoragesNeverMentionsTheAdminAreaToAnyoneElse is the non-disclosure
// half of the table above, asserted on the whole response rather than on the
// Location header alone.
//
// http.Redirect writes the target into the body as well as into the header, so
// a state that leaked the admin area would leak it twice. Only the admin's own
// response may contain it.
func TestNoStoragesNeverMentionsTheAdminAreaToAnyoneElse(t *testing.T) {
	t.Parallel()

	f := newNavFixture(t)

	for name, session := range map[string]string{
		"no session":                  "",
		"a member":                    f.session.ID,
		"an admin who is a member":    f.adminWithSes.ID,
		"a non-admin with no storage": f.plainNoStorageSes.ID,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := sendAs(f.router, session, http.MethodGet, "/no-storages", "")

			assert.NotContains(t, rec.Header().Get("Location"), "admin")
			assert.NotContains(t, rec.Body.String(), "admin")
		})
	}

	// The loop above would pass on a route that says it to nobody at all, so
	// prove the one caller who may hear it does.
	admin := sendAs(f.router, f.adminNoStorageSes.ID, http.MethodGet, "/no-storages", "")
	assert.Equal(t, "/admin", admin.Header().Get("Location"))
}

// TestNoStoragesRereadsIsAdminOnEveryCall is the invariant of
// docs/specs/03-auth-and-multi-tenancy.md applied to this route: revoking
// someone's admin rights takes effect on their next request, not whenever
// their session happens to expire.
//
// One session throughout. Nothing is re-issued, nothing is logged out, and the
// cookie the second call sends is the one the first call sent.
func TestNoStoragesRereadsIsAdminOnEveryCall(t *testing.T) {
	t.Parallel()

	f := newNavFixture(t)

	before := f.auth.adminCallCount()

	first := sendAs(f.router, f.adminNoStorageSes.ID, http.MethodGet, "/no-storages", "")
	require.Equal(t, http.StatusFound, first.Code)
	require.Equal(t, "/admin", first.Header().Get("Location"))

	// Straight into the store, the way an admin revoking rights in the admin
	// area would — the session row is untouched.
	f.auth.setAdmin(f.adminNoStorage.ID, false)

	second := sendAs(f.router, f.adminNoStorageSes.ID, http.MethodGet, "/no-storages", "")
	require.Equal(t, http.StatusFound, second.Code)
	assert.Equal(t, "/storages.html?empty=1", second.Header().Get("Location"),
		"the flag was revoked in the store, so the very next call must route them as a non-admin")

	// Without this the test would also pass on a route that read is_admin once
	// and cached it: what is asserted here is that the route asked the store
	// again, not merely that the answer it gave changed.
	assert.Equal(t, before+2, f.auth.adminCallCount(),
		"is_admin must be read from the store on each of the two calls")
}

// TestNoStoragesRefusesEveryOtherMethodLikeAnUnknownPath — the spec says GET
// only, and "any other method gets the standard 404 envelope of every unrouted
// request".
//
// The static tree is mounted because production mounts one: the route is
// registered for GET alone, so every other verb falls through to staticHandler
// and gets literally the answer an unrouted path gets. The assertions are the
// two halves of "literally": in production the bodies are byte-identical, and
// in dev — the only place debug_reason exists, and so the only place a
// difference could survive — they differ by nothing but the path the caller
// asked for.
//
// HEAD is in the list. The spec says GET, without an exception for the verb
// that is a GET with the body left off.
func TestNoStoragesRefusesEveryOtherMethodLikeAnUnknownPath(t *testing.T) {
	t.Parallel()

	staticTree := func(d *httpapi.Deps) {
		d.StaticFS = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}}
	}
	prod := newNavFixture(t, staticTree)
	dev := newNavFixture(t, staticTree, func(d *httpapi.Deps) {
		d.Errors = httpapi.NewErrorWriter(true, discardLogger())
	})

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			// The reference is the same method against a path that does not
			// exist. Comparing the route against itself would prove nothing.
			reference := sendAs(prod.router, prod.adminNoStorageSes.ID, method, "/no-such-page", "")
			require.Equal(t, http.StatusNotFound, reference.Code)

			rec := sendAs(prod.router, prod.adminNoStorageSes.ID, method, "/no-storages", "")
			assert.Equal(t, reference.Code, rec.Code)
			assert.Equal(t, reference.Body.String(), rec.Body.String())
			assert.Equal(t, reference.Header().Get("Content-Type"), rec.Header().Get("Content-Type"))
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			assert.Empty(t, rec.Header().Get("Location"), "a refused verb is not redirected anywhere")

			devReference := sendAs(dev.router, dev.adminNoStorageSes.ID, method, "/no-such-page", "")
			devRec := sendAs(dev.router, dev.adminNoStorageSes.ID, method, "/no-storages", "")
			require.Contains(t, devReference.Body.String(), "debug_reason",
				"the dev fixture must actually be in dev, or this half asserts nothing")
			assert.Equal(t,
				strings.Replace(devReference.Body.String(), "/no-such-page", "/no-storages", 1),
				devRec.Body.String(),
				"in dev the two answers may differ only by the path the caller asked for")
		})
	}
}

// TestNoStoragesRefusesAWrongMethodWithoutConsultingTheSession — "any method
// other than GET is a 404" carries no "…if you are logged in" clause. An
// unauthenticated POST gets the 404, not the redirect an unauthenticated GET
// gets, because it never reaches the session gate at all: it is not routed
// here in the first place.
func TestNoStoragesRefusesAWrongMethodWithoutConsultingTheSession(t *testing.T) {
	t.Parallel()

	f := newNavFixture(t, func(d *httpapi.Deps) {
		d.StaticFS = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}}
	})

	rec := sendAs(f.router, "", http.MethodPost, "/no-storages", "")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"))
}

// TestNoStoragesNeverSendsAPairedDeviceToTheAdminArea.
//
// docs/specs/12-client-api-contract.md: "a paired client is never an admin
// client", and RequireAdmin refuses a device session whatever its user's
// is_admin says. This route has to agree. If it did not, the Location would be
// an is_admin oracle over the one transport the admin area excludes — and
// following it would land the device on the 404 that exclusion produces, which
// is a dead end rather than guidance.
func TestNoStoragesNeverSendsAPairedDeviceToTheAdminArea(t *testing.T) {
	t.Parallel()

	f := newNavFixture(t)
	device := f.auth.addDeviceSession(f.adminNoStorage.ID, "phone")

	browser := sendAs(f.router, f.adminNoStorageSes.ID, http.MethodGet, "/no-storages", "")
	require.Equal(t, "/admin", browser.Header().Get("Location"),
		"the same user's browser session must reach the admin area, or this test proves nothing")

	rec := sendAs(f.router, device.ID, http.MethodGet, "/no-storages", "")

	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/storages.html?empty=1", rec.Header().Get("Location"),
		"a paired device sees exactly what a non-admin sees")
	assert.NotContains(t, rec.Body.String(), "admin")
}

// TestNoStoragesLeavesTheApiSayingNothingAboutAdmins.
//
// The whole design rests on the client never learning what the server decided,
// so this asserts on the response the client actually reads: GET /api/auth/me
// carries exactly {id, username, display_name, storages}, and is identical in
// shape for an admin and a non-admin in the same situation.
func TestNoStoragesLeavesTheApiSayingNothingAboutAdmins(t *testing.T) {
	t.Parallel()

	f := newNavFixture(t)

	keysOf := func(t *testing.T, sessionID string) []string {
		t.Helper()
		rec := sendAs(f.router, sessionID, http.MethodGet, "/api/auth/me", "")
		require.Equal(t, http.StatusOK, rec.Code)

		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		keys := make([]string, 0, len(body))
		for key := range body {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys
	}

	want := []string{"display_name", "id", "storages", "username"}
	adminKeys := keysOf(t, f.adminNoStorageSes.ID)
	plainKeys := keysOf(t, f.plainNoStorageSes.ID)

	assert.Equal(t, want, adminKeys)
	assert.Equal(t, want, plainKeys)
	assert.Equal(t, plainKeys, adminKeys,
		"two users the route sends to different places must be indistinguishable to the client")
}

// brokenSessions is a store whose session lookup fails the way a database that
// is down fails — not with ErrNotFound, which is a caller condition, but with
// an error nobody can act on.
type brokenSessions struct{ *fakeAuth }

func (brokenSessions) LookupSession(context.Context, string) (*store.Session, error) {
	return nil, errors.New("connection refused")
}

// TestRequireSessionRedirectDoesNotTurnAnOutageIntoALogin.
//
// Only an *unauthorized* refusal becomes the redirect. A store failure stays a
// 500 envelope, as it is everywhere else in the package. Sending the visitor
// to the login page would tell them their session had expired; they would log
// in again, hit the same outage, and the real cause would have been shown to
// nobody.
func TestRequireSessionRedirectDoesNotTurnAnOutageIntoALogin(t *testing.T) {
	t.Parallel()

	errs := httpapi.NewErrorWriter(false, discardLogger())
	mw := httpapi.NewMiddleware(brokenSessions{newFakeAuth()}, errs)

	var reached bool
	handler := mw.RequireSessionRedirect(httpapi.LoginPage)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/no-storages", nil)
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: "sess-" + uuid.NewString()})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.False(t, reached, "a request that could not be resolved must not reach the handler")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"))
	assert.NotContains(t, rec.Body.String(), "connection refused", "no reason leaks in production")
}
