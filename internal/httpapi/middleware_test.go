package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeAuth is an in-memory AuthStore, so the authorization rules are exercised
// without a database and — more importantly — so a test can change the answers
// mid-flight the way an admin revoking rights would.
type fakeAuth struct {
	mu sync.Mutex

	sessions map[string]*store.Session
	users    map[uuid.UUID]*store.User
	admins   map[uuid.UUID]bool
	members  map[string]bool // storageID+userID

	isAdminCalls int
	touchCalls   int
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{
		sessions: map[string]*store.Session{},
		users:    map[uuid.UUID]*store.User{},
		admins:   map[uuid.UUID]bool{},
		members:  map[string]bool{},
	}
}

func (f *fakeAuth) addUser(t *testing.T, isAdmin bool) (*store.User, *store.Session) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	id := uuid.New()
	user := &store.User{ID: id, Username: "u" + id.String()[:6], DisplayName: "User", IsAdmin: isAdmin}
	session := &store.Session{ID: "sess-" + id.String(), UserID: id, Kind: store.SessionBrowser, ExpiresAt: time.Now().Add(time.Hour)}

	f.users[id] = user
	f.sessions[session.ID] = session
	f.admins[id] = isAdmin
	return user, session
}

func (f *fakeAuth) setAdmin(id uuid.UUID, isAdmin bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admins[id] = isAdmin
}

func (f *fakeAuth) addMember(storageID, userID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[storageID.String()+userID.String()] = true
}

func (f *fakeAuth) adminCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isAdminCalls
}

func (f *fakeAuth) LookupSession(_ context.Context, id string) (*store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[id]; ok {
		return s, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAuth) UserByID(_ context.Context, id uuid.UUID) (*store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAuth) IsAdmin(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isAdminCalls++
	return f.admins[id], nil
}

func (f *fakeAuth) IsStorageMember(_ context.Context, storageID, userID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.members[storageID.String()+userID.String()], nil
}

func (f *fakeAuth) TouchSession(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touchCalls++
	return nil
}

// testRouter wires the three gates the way the application does.
func testRouter(dev bool, auth *fakeAuth) http.Handler {
	errs := httpapi.NewErrorWriter(dev, discardLogger())
	mw := httpapi.NewMiddleware(auth, errs)

	ok := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}

	r := chi.NewRouter()
	r.Route("/api/admin", func(admin chi.Router) {
		admin.Use(mw.RequireSession, mw.RequireAdmin)
		admin.Get("/users", ok)
	})
	r.Route("/admin", func(admin chi.Router) {
		admin.Use(mw.RequireSession, mw.RequireAdmin)
		admin.Get("/", ok)
	})
	r.Route("/api/storages/{storage_id}", func(scoped chi.Router) {
		scoped.Use(mw.RequireSession, mw.RequireStorageMember)
		scoped.Get("/products", ok)
	})
	return r
}

func get(t *testing.T, h http.Handler, path, sessionID string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if sessionID != "" {
		req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: sessionID})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestEveryRefusalIsTheSame404 is the non-enumeration rule end to end.
//
// A storage that does not exist, a storage the caller is not a member of, an
// id that is not even a UUID, and the entire admin area must all answer with
// the same `404` — same status, same body, same headers. Any difference lets
// someone probe ids and learn which ones name real storages, which is exactly
// the inference the rule exists to prevent.
func TestEveryRefusalIsTheSame404(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	user, session := auth.addUser(t, false)
	mine := uuid.New()
	auth.addMember(mine, user.ID)

	theirs := uuid.New() // a real storage elsewhere, as far as this user knows
	router := testRouter(false, auth)

	responses := map[string]*httptest.ResponseRecorder{
		"storage the caller is not in": get(t, router, "/api/storages/"+theirs.String()+"/products", session.ID),
		"storage that does not exist":  get(t, router, "/api/storages/"+uuid.New().String()+"/products", session.ID),
		"malformed storage id":         get(t, router, "/api/storages/not-a-uuid/products", session.ID),
		"admin json route":             get(t, router, "/api/admin/users", session.ID),
	}

	var reference *httptest.ResponseRecorder
	var referenceName string
	for name, rec := range responses {
		if reference == nil {
			reference, referenceName = rec, name
			continue
		}
		assert.Equalf(t, http.StatusNotFound, rec.Code, "%s must be 404", name)
		assert.Equalf(t, reference.Body.Bytes(), rec.Body.Bytes(),
			"%q and %q must be byte-identical", name, referenceName)
		assert.Equalf(t, reference.Header(), rec.Header(),
			"%q and %q must have identical headers", name, referenceName)
	}
	require.Equal(t, http.StatusNotFound, reference.Code)

	// And the member's own storage still works, so the gate is not simply
	// refusing everything.
	okRec := get(t, router, "/api/storages/"+mine.String()+"/products", session.ID)
	assert.Equal(t, http.StatusOK, okRec.Code)
}

// TestNoRefusalUses403 pins the choice of status code itself. 403 is the
// obvious thing to reach for and is precisely what must not appear.
func TestNoRefusalUses403(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	_, session := auth.addUser(t, false)
	router := testRouter(false, auth)

	for _, path := range []string{
		"/api/storages/" + uuid.New().String() + "/products",
		"/api/admin/users",
		"/admin/",
	} {
		rec := get(t, router, path, session.ID)
		assert.NotEqualf(t, http.StatusForbidden, rec.Code,
			"%s answered 403; the spec forbids it for storage and admin scoping", path)
		assert.Equalf(t, http.StatusNotFound, rec.Code, "%s", path)
	}
}

// TestAdminRightsAreRecheckedOnEveryRequest is the invariant that a cached
// flag breaks silently.
//
// The same session is used throughout. Admin rights are revoked between
// requests, the way an admin removing someone's access would, and the very
// next request must be refused — without the user having to log out, and
// without waiting for their session to expire.
func TestAdminRightsAreRecheckedOnEveryRequest(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	user, session := auth.addUser(t, true)
	router := testRouter(false, auth)

	first := get(t, router, "/api/admin/users", session.ID)
	require.Equal(t, http.StatusOK, first.Code, "an admin gets in")

	auth.setAdmin(user.ID, false) // revoked out-of-band

	second := get(t, router, "/api/admin/users", session.ID)
	assert.Equal(t, http.StatusNotFound, second.Code,
		"the next request on the same session must be refused; a cached flag would still let them in")

	auth.setAdmin(user.ID, true) // granted again

	third := get(t, router, "/api/admin/users", session.ID)
	assert.Equal(t, http.StatusOK, third.Code, "and a re-grant must take effect just as fast")

	assert.Equal(t, 3, auth.adminCallCount(),
		"the database is asked once per request, not once per session")
}

// TestBothAdminSurfacesShareTheGate — the HTML pages are not a weaker path.
// Registering them on their own router with their own check is how one of the
// two ends up unprotected.
func TestBothAdminSurfacesShareTheGate(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	_, session := auth.addUser(t, false)
	router := testRouter(false, auth)

	jsonRec := get(t, router, "/api/admin/users", session.ID)
	htmlRec := get(t, router, "/admin/", session.ID)

	assert.Equal(t, http.StatusNotFound, jsonRec.Code)
	assert.Equal(t, http.StatusNotFound, htmlRec.Code)
	assert.Equal(t, jsonRec.Body.Bytes(), htmlRec.Body.Bytes(),
		"both admin surfaces must refuse identically")
}

// TestIsAdminNeverReachesTheClient — the flag is a server-side fact. It must
// not appear in a refusal, and it must not appear in a success.
func TestIsAdminNeverReachesTheClient(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	_, adminSession := auth.addUser(t, true)
	router := testRouter(false, auth)

	rec := get(t, router, "/api/admin/users", adminSession.ID)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "is_admin")

	refusal := get(t, router, "/api/storages/"+uuid.New().String()+"/products", adminSession.ID)
	assert.NotContains(t, refusal.Body.String(), "is_admin")
}

// TestSessionTransports covers the two ways a session id arrives, and which
// one wins.
func TestSessionTransports(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	cookieUser, cookieSession := auth.addUser(t, false)
	bearerUser, bearerSession := auth.addUser(t, false)

	storageID := uuid.New()
	auth.addMember(storageID, cookieUser.ID)
	// The bearer user is deliberately NOT a member.
	_ = bearerUser

	router := testRouter(false, auth)
	path := "/api/storages/" + storageID.String() + "/products"

	t.Run("bearer alone is accepted", func(t *testing.T) {
		auth.addMember(storageID, bearerUser.ID)
		defer func() { auth.members[storageID.String()+bearerUser.ID.String()] = false }()

		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+bearerSession.ID)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code, "a native client has no cookie to send")
	})

	t.Run("cookie wins when both are present", func(t *testing.T) {
		// The cookie's user is a member; the header names a session that is
		// not. If the header won, this would succeed.
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: cookieSession.ID})
		req.Header.Set("Authorization", "Bearer "+bearerSession.ID)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code,
			"the cookie must decide; letting a header override a logged-in browser is the attack this prevents")
	})
}

func TestMissingOrUnknownSessionIs401(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	router := testRouter(false, auth)
	path := "/api/storages/" + uuid.New().String() + "/products"

	none := get(t, router, path, "")
	assert.Equal(t, http.StatusUnauthorized, none.Code)

	unknown := get(t, router, path, "sess-does-not-exist")
	assert.Equal(t, http.StatusUnauthorized, unknown.Code)

	assert.Equal(t, none.Body.Bytes(), unknown.Body.Bytes(),
		"a missing session and an unknown one are the same answer")
}

// TestDevRevealsWhichCheckFailed is the other side of the gating: in dev the
// distinction the production response hides is available, which is what makes
// the opaque 404 tolerable to work with.
func TestDevRevealsWhichCheckFailed(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	user, session := auth.addUser(t, false)
	storageID := uuid.New()
	auth.addMember(storageID, user.ID)

	router := testRouter(true, auth) // dev

	notMember := get(t, router, "/api/storages/"+uuid.New().String()+"/products", session.ID)
	assert.Contains(t, notMember.Body.String(), httpapi.ReasonNotStorageMember)

	malformed := get(t, router, "/api/storages/nonsense/products", session.ID)
	assert.Contains(t, malformed.Body.String(), httpapi.ReasonStorageNotFound)

	notAdmin := get(t, router, "/api/admin/users", session.ID)
	assert.Contains(t, notAdmin.Body.String(), httpapi.ReasonNotAdmin)

	// The same three under production reveal none of it.
	prod := testRouter(false, auth)
	for _, path := range []string{
		"/api/storages/" + uuid.New().String() + "/products",
		"/api/storages/nonsense/products",
		"/api/admin/users",
	} {
		rec := get(t, prod, path, session.ID)
		for _, reason := range []string{
			httpapi.ReasonNotStorageMember,
			httpapi.ReasonStorageNotFound,
			httpapi.ReasonNotAdmin,
		} {
			assert.NotContainsf(t, rec.Body.String(), reason, "%s leaked %s", path, reason)
		}
	}
}

// TestHandlersReadTheValidatedStorageID — a handler that re-parsed the path
// could act on an id nothing checked.
func TestHandlersReadTheValidatedStorageID(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	user, session := auth.addUser(t, false)
	storageID := uuid.New()
	auth.addMember(storageID, user.ID)

	errs := httpapi.NewErrorWriter(false, discardLogger())
	mw := httpapi.NewMiddleware(auth, errs)

	var seen uuid.UUID
	var found bool
	r := chi.NewRouter()
	r.Route("/api/storages/{storage_id}", func(scoped chi.Router) {
		scoped.Use(mw.RequireSession, mw.RequireStorageMember)
		scoped.Get("/products", func(w http.ResponseWriter, req *http.Request) {
			seen, found = httpapi.StorageIDFrom(req.Context())
			w.WriteHeader(http.StatusOK)
		})
	})

	rec := get(t, r, "/api/storages/"+storageID.String()+"/products", session.ID)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, found, "the validated id must be in the context")
	assert.Equal(t, storageID, seen)
}
