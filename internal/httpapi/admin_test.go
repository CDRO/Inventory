package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// The admin half of the in-memory store. It lives on fakeAuth rather than a
// separate fake because deleting a user has to take their sessions with it,
// and the sessions are fakeAuth's.

func (f *fakeAuth) ListUsers(_ context.Context) ([]store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.User, 0, len(f.users))
	for _, u := range f.users {
		copied := *u
		copied.IsAdmin = f.admins[u.ID]
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (f *fakeAuth) CreateUser(_ context.Context, in store.NewUser) (*store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.Username == in.Username {
			return nil, fmt.Errorf("%w: username %q is taken", store.ErrDuplicate, in.Username)
		}
	}
	u := &store.User{
		ID: uuid.New(), Username: in.Username, PasswordHash: in.PasswordHash,
		DisplayName: in.DisplayName, IsAdmin: in.IsAdmin, CreatedAt: time.Now(),
	}
	f.users[u.ID] = u
	f.admins[u.ID] = in.IsAdmin
	return u, nil
}

// DeleteUser cascades to sessions and memberships, as the schema's foreign keys
// do. The real cascade is pinned against PostgreSQL by the store package's
// TestDeletingAUserRevokesTheirSessionsImmediately; the HTTP test of the same
// name below pins that the route reaches it.
func (f *fakeAuth) DeleteUser(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.users, id)
	delete(f.admins, id)
	for sid, s := range f.sessions {
		if s.UserID == id {
			delete(f.sessions, sid)
		}
	}
	for key := range f.members {
		if strings.HasSuffix(key, id.String()) {
			delete(f.members, key)
		}
	}
	return nil
}

func (f *fakeAuth) ListStorages(_ context.Context) ([]store.Storage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Storage, 0, len(f.storages))
	for _, s := range f.storages {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeAuth) CreateStorage(_ context.Context, name string) (*store.Storage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storages == nil {
		f.storages = map[uuid.UUID]*store.Storage{}
	}
	s := &store.Storage{ID: uuid.New(), Name: name, CreatedAt: time.Now()}
	f.storages[s.ID] = s
	return s, nil
}

func (f *fakeAuth) DeleteStorage(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.storages[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.storages, id)
	for key := range f.members {
		if strings.HasPrefix(key, id.String()) {
			delete(f.members, key)
		}
	}
	return nil
}

func (f *fakeAuth) ListMembers(_ context.Context, storageID uuid.UUID) ([]store.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Member
	for key := range f.members {
		if !strings.HasPrefix(key, storageID.String()) {
			continue
		}
		userID, err := uuid.Parse(strings.TrimPrefix(key, storageID.String()))
		if err != nil {
			continue
		}
		if u, ok := f.users[userID]; ok {
			out = append(out, store.Member{UserID: u.ID, Username: u.Username, DisplayName: u.DisplayName})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

// AddMember mirrors the store: idempotent, and a missing user or storage is
// ErrNotFound (the foreign keys' answer).
func (f *fakeAuth) AddMember(_ context.Context, storageID, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, storageOK := f.storages[storageID]
	_, userOK := f.users[userID]
	if !storageOK || !userOK {
		return store.ErrNotFound
	}
	f.members[storageID.String()+userID.String()] = true
	return nil
}

func (f *fakeAuth) RemoveMember(_ context.Context, storageID, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := storageID.String() + userID.String()
	if !f.members[key] {
		return store.ErrNotFound
	}
	delete(f.members, key)
	return nil
}

// newAdminFixture is newAPIFixture with the caller promoted to admin.
func newAdminFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	f.auth.setAdmin(f.user.ID, true)
	f.user.IsAdmin = true
	return f
}

func sendAs(router http.Handler, sessionID, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if sessionID != "" {
		req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: sessionID})
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestNonAdminCannotTellTheAdminAreaExists is the admin non-disclosure rule
// end to end, on the real router with a static tree mounted the way production
// mounts one.
//
// Every admin route a non-admin hits — HTML or JSON, any verb — must be
// byte-identical, status and headers included, to a path that does not exist at
// all. Comparing against a real miss is the point: two admin routes agreeing
// with each other proves nothing if both differ from /nonexistent, because that
// difference is the map of the admin area.
func TestNonAdminCannotTellTheAdminAreaExists(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	_, session := auth.addUser(t, false)
	victim, _ := auth.addUser(t, false)
	storage, err := auth.CreateStorage(context.Background(), "Pantry")
	require.NoError(t, err)

	router := httpapi.NewRouter(httpapi.Deps{
		DB:     stubPinger{},
		Vision: stubVision{status: "ok"},
		Errors: httpapi.NewErrorWriter(false, discardLogger()),
		Store: fakeAPI{
			fakeAuth: auth, fakeLocations: &fakeLocations{}, fakeBatches: &fakeBatches{},
			fakeShoppingLists: &fakeShoppingLists{}, fakeExpiry: &fakeExpiry{},
		},
		StaticFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}},
	})

	reference := sendAs(router, session.ID, http.MethodGet, "/no-such-page", "")
	require.Equal(t, http.StatusNotFound, reference.Code)

	requests := []struct{ method, path, body string }{
		// The admin area.
		{http.MethodGet, "/admin", ""},
		{http.MethodGet, "/api/admin/users", ""},
		{http.MethodPost, "/api/admin/users", `{"username":"mallory","password":"password123","is_admin":true}`},
		{http.MethodDelete, "/api/admin/users/" + victim.ID.String(), ""},
		{http.MethodGet, "/api/admin/storages", ""},
		{http.MethodDelete, "/api/admin/storages/" + storage.ID.String(), ""},
		{http.MethodPost, "/api/admin/storages/" + storage.ID.String() + "/members", `{"user_id":"` + victim.ID.String() + `"}`},
		// A registered admin path with a verb it does not have: chi would say
		// 405 here if the static catch-all did not claim it first.
		{http.MethodPut, "/api/admin/users", ""},
		// Paths that do not exist, near and far.
		{http.MethodGet, "/adminx", ""},
		{http.MethodGet, "/admin/users", ""},
		{http.MethodGet, "/api/admin/nonexistent", ""},
		{http.MethodPost, "/api/admin/nonexistent", "{}"},
		{http.MethodDelete, "/api/nothing/here", ""},
	}

	for _, tc := range requests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := sendAs(router, session.ID, tc.method, tc.path, tc.body)

			assert.Equal(t, reference.Code, rec.Code)
			assert.Equal(t, reference.Body.String(), rec.Body.String())
			assert.Equal(t, reference.Header(), rec.Header())
		})
	}

	_, stillThere := auth.users[victim.ID]
	assert.True(t, stillThere, "a refused delete must not have deleted anything")
	for _, u := range auth.users {
		assert.NotEqual(t, "mallory", u.Username, "a refused create must not have created anything")
	}
}

// TestAdminUserListNeverCarriesIsAdmin — not even here. The spec says "never
// returned by any JSON API", and an admin-only endpoint is still a JSON API a
// client could read and branch on.
func TestAdminUserListNeverCarriesIsAdmin(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	created := f.do(http.MethodPost, "/api/admin/users",
		`{"username":"root2","password":"long enough","display_name":"Second admin","is_admin":true}`)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	assert.NotContains(t, created.Body.String(), "is_admin")
	assert.NotContains(t, created.Body.String(), "password")

	var user struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &user))
	isAdmin, err := f.auth.IsAdmin(context.Background(), user.ID)
	require.NoError(t, err)
	assert.True(t, isAdmin, "is_admin is accepted as input even though it is never output")

	list := f.do(http.MethodGet, "/api/admin/users", "")
	require.Equal(t, http.StatusOK, list.Code)
	assert.NotContains(t, list.Body.String(), "is_admin")
	assert.NotContains(t, list.Body.String(), "admin\":", "no admin flag under any name")
	assert.NotContains(t, list.Body.String(), "password")
	assert.Contains(t, list.Body.String(), "root2")
}

func TestCreateUserValidatesItsInput(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	rec := f.do(http.MethodPost, "/api/admin/users", `{"username":"  ","password":"short"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	var body struct {
		Error struct {
			Fields map[string][]string `json:"fields"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body.Error.Fields, "username")
	assert.Contains(t, body.Error.Fields, "password")

	first := f.do(http.MethodPost, "/api/admin/users", `{"username":"anna","password":"long enough"}`)
	require.Equal(t, http.StatusCreated, first.Code)
	assert.Contains(t, first.Body.String(), `"display_name":"anna"`, "display name defaults to the username")

	dup := f.do(http.MethodPost, "/api/admin/users", `{"username":"anna","password":"long enough"}`)
	require.Equal(t, http.StatusConflict, dup.Code)
	assert.Contains(t, dup.Body.String(), "That username is already taken.")
	assert.NotContains(t, dup.Body.String(), "store:", "the store's own error text stays out of a production response")
}

// TestDeletingAUserRevokesTheirSessionsImmediately — the next request with
// their cookie is a 401, not a request that works until the session expires.
func TestDeletingAUserRevokesTheirSessionsImmediately(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	victim, victimSession := f.auth.addUser(t, false)

	require.Equal(t, http.StatusOK, sendAs(f.router, victimSession.ID, http.MethodGet, "/api/auth/me", "").Code)

	rec := f.do(http.MethodDelete, "/api/admin/users/"+victim.ID.String(), "")
	require.Equal(t, http.StatusNoContent, rec.Code)

	assert.Equal(t, http.StatusUnauthorized,
		sendAs(f.router, victimSession.ID, http.MethodGet, "/api/auth/me", "").Code)
	assert.Equal(t, http.StatusNotFound,
		f.do(http.MethodDelete, "/api/admin/users/"+victim.ID.String(), "").Code,
		"deleting again is a 404")
}

// TestAdminCannotDeleteThemselves — on a single-admin install that would lock
// the household out of its own server.
func TestAdminCannotDeleteThemselves(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	rec := f.do(http.MethodDelete, "/api/admin/users/"+f.user.ID.String(), "")
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, http.StatusOK, f.do(http.MethodGet, "/api/auth/me", "").Code, "still signed in")
}

func TestAdminStorageAndMembershipLifecycle(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	member, memberSession := f.auth.addUser(t, false)

	require.Equal(t, http.StatusUnprocessableEntity,
		f.do(http.MethodPost, "/api/admin/storages", `{"name":"   "}`).Code)

	created := f.do(http.MethodPost, "/api/admin/storages", `{"name":"Cellar"}`)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var storage struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &storage))
	assert.Equal(t, "Cellar", storage.Name)

	list := f.do(http.MethodGet, "/api/admin/storages", "")
	require.Equal(t, http.StatusOK, list.Code)
	assert.Contains(t, list.Body.String(), storage.ID.String(), "the admin list is the one global storage list")

	membersPath := "/api/admin/storages/" + storage.ID.String() + "/members"
	grant := `{"user_id":"` + member.ID.String() + `"}`

	require.Equal(t, http.StatusNoContent, f.do(http.MethodPost, membersPath, grant).Code)
	require.Equal(t, http.StatusNoContent, f.do(http.MethodPost, membersPath, grant).Code, "granting twice is idempotent")

	members := f.do(http.MethodGet, membersPath, "")
	require.Equal(t, http.StatusOK, members.Code)
	assert.Contains(t, members.Body.String(), member.ID.String())

	// The grant is real access: the member's own /me now lists it.
	me := sendAs(f.router, memberSession.ID, http.MethodGet, "/api/auth/me", "")
	assert.Contains(t, me.Body.String(), storage.ID.String())

	assert.Equal(t, http.StatusNotFound,
		f.do(http.MethodPost, membersPath, `{"user_id":"`+uuid.NewString()+`"}`).Code, "unknown user")
	assert.Equal(t, http.StatusUnprocessableEntity,
		f.do(http.MethodPost, membersPath, `{"user_id":"not-a-uuid"}`).Code)
	assert.Equal(t, http.StatusNotFound,
		f.do(http.MethodPost, "/api/admin/storages/"+uuid.NewString()+"/members", grant).Code, "unknown storage")

	revoke := membersPath + "/" + member.ID.String()
	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, revoke, "").Code)
	assert.Equal(t, http.StatusNotFound, f.do(http.MethodDelete, revoke, "").Code, "revoking twice is a 404")
	me = sendAs(f.router, memberSession.ID, http.MethodGet, "/api/auth/me", "")
	assert.NotContains(t, me.Body.String(), storage.ID.String(), "revocation is immediate")

	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/api/admin/storages/"+storage.ID.String(), "").Code)
	assert.Equal(t, http.StatusNotFound, f.do(http.MethodDelete, "/api/admin/storages/"+storage.ID.String(), "").Code)
}

// TestAdminPageRendersServerSide covers the HTML half: it shows admin status
// (the one place it may appear), escapes what users typed, withholds the
// delete action from the caller's own row, and locks itself down with a
// nonce-based CSP whose nonce actually matches the page's script.
func TestAdminPageRendersServerSide(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	created := f.do(http.MethodPost, "/api/admin/users",
		`{"username":"eve","password":"long enough","display_name":"<script>alert(1)</script>"}`)
	require.Equal(t, http.StatusCreated, created.Code)
	require.Equal(t, http.StatusCreated, f.do(http.MethodPost, "/api/admin/storages", `{"name":"Garage"}`).Code)

	rec := f.do(http.MethodGet, "/admin", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	page := rec.Body.String()
	assert.Contains(t, page, f.user.Username)
	assert.Contains(t, page, "eve")
	assert.Contains(t, page, "Garage")
	assert.NotContains(t, page, "<script>alert(1)</script>", "user-typed text must be escaped")
	assert.Contains(t, page, "&lt;script&gt;alert(1)&lt;/script&gt;")
	assert.NotContains(t, page, "/api/admin/users/"+f.user.ID.String(), "no delete action on your own row")

	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "frame-ancestors 'none'")
	match := regexp.MustCompile(`script-src 'nonce-([^']+)'`).FindStringSubmatch(csp)
	require.Len(t, match, 2, "the CSP must pin scripts to a nonce: %q", csp)
	assert.Contains(t, page, `<script nonce="`+match[1]+`">`)

	again := f.do(http.MethodGet, "/admin", "")
	assert.NotEqual(t, csp, again.Header().Get("Content-Security-Policy"), "the nonce is per request")

	// Demoted mid-session: the very next page load is the hidden-area 404.
	f.auth.setAdmin(f.user.ID, false)
	assert.Equal(t, http.StatusNotFound, f.do(http.MethodGet, "/admin", "").Code)
}
