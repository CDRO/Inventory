package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/CDRO/Inventory/internal/config"
	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// fakeAdminVision is the fuller vision checker the admin settings routes and
// the /admin AI-model banner need, beyond stubVision's bare Status.
type fakeAdminVision struct {
	model       string
	modelErr    error
	models      []string
	modelsErr   error
	status      string
	invalidated int
}

func (f *fakeAdminVision) EffectiveModel(context.Context) (string, error) {
	return f.model, f.modelErr
}

func (f *fakeAdminVision) Models(context.Context) ([]string, error) {
	return f.models, f.modelsErr
}

func (f *fakeAdminVision) Status(context.Context) string {
	return f.status
}

func (f *fakeAdminVision) Invalidate() {
	f.invalidated++
}

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

func (f *fakeAuth) Setting(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.settings[key]
	return v, ok, nil
}

func (f *fakeAuth) SetSetting(_ context.Context, key, value string, _ uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[key] = value
	return nil
}

func (f *fakeAuth) SearchCatalog(_ context.Context, q string) ([]store.CatalogProduct, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.CatalogProduct, 0)
	for _, c := range f.catalog {
		if q == "" || strings.Contains(strings.ToLower(c.DisplayName), strings.ToLower(q)) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeAuth) CorrectCatalogShelfLife(_ context.Context, id uuid.UUID, days *int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.catalog {
		if c.ID == id {
			f.catalog[i].DefaultShelfLifeDays = days
			f.catalogRecomputeCalls = append(f.catalogRecomputeCalls, id)
			return f.catalogRecompute[id], nil
		}
	}
	return 0, store.ErrNotFound
}

func (f *fakeAuth) DeleteCatalogProduct(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.catalog {
		if c.ID == id {
			f.catalog = append(f.catalog[:i], f.catalog[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

// newAdminFixture is newAPIFixture with the caller promoted to admin.
func newAdminFixture(t *testing.T, opts ...func(*httpapi.Deps)) *apiFixture {
	t.Helper()
	f := newAPIFixture(t, opts...)
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
		Store:  newFakeAPI(auth),
		// AdminVision and Config are set so every conditionally-registered
		// admin route — the env-file download in particular — actually
		// exists in this router, and is proven hidden rather than merely
		// untested (docs/specs/03-auth-and-multi-tenancy.md's non-disclosure
		// rule; the env-file route serves every secret in the deployment).
		AdminVision: &fakeAdminVision{status: "ok"},
		Config:      &config.Config{GeminiModel: "gemini-2.0-flash", AppEnv: "dev", HTTPPort: "8000"},
		StaticFS:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}},
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
		{http.MethodGet, "/api/admin/settings", ""},
		{http.MethodPut, "/api/admin/settings", `{"gemini_model":"gemini-2.5-flash"}`},
		// Registered only conditionally (router.go, when Deps.Config is set) and
		// the one route that serves every secret in the deployment verbatim —
		// the route a regression here would be most costly to expose.
		{http.MethodGet, "/api/admin/settings/env-file", ""},
		{http.MethodGet, "/api/admin/catalog", ""},
		{http.MethodPatch, "/api/admin/catalog/" + uuid.New().String(), `{"default_shelf_life_days":5}`},
		{http.MethodDelete, "/api/admin/catalog/" + uuid.New().String(), ""},
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

// TestGetSettingsReportsEffectiveModelAndAvailability covers GET
// /api/admin/settings: the effective model, whether the provider currently
// offers it, and the full list a picker would need
// (docs/specs/01-architecture-and-deployment.md's AI model resilience).
func TestGetSettingsReportsEffectiveModelAndAvailability(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	f.adminVision.model = "gemini-2.0-flash"
	f.adminVision.status = "model_unavailable"
	f.adminVision.models = []string{"gemini-2.5-flash", "gemini-2.5-pro"}

	rec := f.do(http.MethodGet, "/api/admin/settings", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"gemini_model":"gemini-2.0-flash"`)
	assert.Contains(t, rec.Body.String(), `"status":"model_unavailable"`)
	assert.Contains(t, rec.Body.String(), "gemini-2.5-flash")
	assert.Contains(t, rec.Body.String(), "gemini-2.5-pro")
}

// TestGetSettingsSurvivesAnUnreachableProvider: found by the E2E suite's
// first real run against a fake, unauthenticated Gemini API key (issue #62)
// — Models() failing (a provider outage, a bad key, no network) must not
// take the whole settings route down with it, since "the model list could
// not be fetched" is exactly the condition Status already reports as
// model_unavailable rather than as an error.
func TestGetSettingsSurvivesAnUnreachableProvider(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	f.adminVision.model = "gemini-2.0-flash"
	f.adminVision.status = "model_unavailable"
	f.adminVision.modelsErr = errors.New("vision: list models: connect: no route to host")

	rec := f.do(http.MethodGet, "/api/admin/settings", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"gemini_model":"gemini-2.0-flash"`)
	assert.Contains(t, rec.Body.String(), `"available_models":[]`)
}

// TestPutSettingsWritesAndInvalidatesCache: the write must reach the
// settings-table override under vision.SettingsModelKey, and the cached
// model list must be dropped so the very next check reflects it — "applied
// immediately, no restart" is the point of the override.
func TestPutSettingsWritesAndInvalidatesCache(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	rec := f.do(http.MethodPut, "/api/admin/settings", `{"gemini_model":"gemini-2.5-flash"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "gemini-2.5-flash")

	value, ok, err := f.auth.Setting(context.Background(), "gemini_model")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "gemini-2.5-flash", value)
	assert.Equal(t, 1, f.adminVision.invalidated, "the cached model list must be invalidated on write")
}

// TestPutSettingsRejectsEmptyModel: a blank model id would make
// EffectiveModel return "" and Status permanently model_unavailable — a 422
// naming the field, not a write that quietly breaks vision for everyone.
func TestPutSettingsRejectsEmptyModel(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodPut, "/api/admin/settings", `{"gemini_model":"  "}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, 0, f.adminVision.invalidated)
}

// TestEnvFileDownloadsARegeneratedEnv covers the "Admin remediation" path in
// docs/specs/01-architecture-and-deployment.md: the file must be offered as
// a download and must carry the *effective* model — the settings-table
// override, not the stale environment value — since a file that still named
// the dead model would not fix anything on redeploy.
func TestEnvFileDownloadsARegeneratedEnv(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	f.adminVision.model = "gemini-2.5-flash" // overridden away from the fixture's env default

	rec := f.do(http.MethodGet, "/api/admin/settings/env-file", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, rec.Body.String(), "GEMINI_MODEL=gemini-2.5-flash")
	assert.NotContains(t, rec.Body.String(), "GEMINI_MODEL=gemini-2.0-flash",
		"the stale env-default model must not appear once a settings override is effective")
}

// TestSearchCatalogFiltersByQuery covers GET /api/admin/catalog?q=, and
// carries id — unlike every storage-facing view of catalog_products
// (docs/specs/02-data-model.md) — because PATCH/DELETE need it.
func TestSearchCatalogFiltersByQuery(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	milk := uuid.New()
	bread := uuid.New()
	f.auth.catalog = []store.CatalogProduct{
		{ID: milk, DisplayName: "Whole Milk", ItemType: store.ItemPerishable},
		{ID: bread, DisplayName: "Sourdough Bread", ItemType: store.ItemPerishable},
	}

	all := f.do(http.MethodGet, "/api/admin/catalog", "")
	require.Equal(t, http.StatusOK, all.Code)
	assert.Contains(t, all.Body.String(), "Whole Milk")
	assert.Contains(t, all.Body.String(), "Sourdough Bread")
	assert.Contains(t, all.Body.String(), milk.String())

	filtered := f.do(http.MethodGet, "/api/admin/catalog?q=milk", "")
	require.Equal(t, http.StatusOK, filtered.Code)
	assert.Contains(t, filtered.Body.String(), "Whole Milk")
	assert.NotContains(t, filtered.Body.String(), "Sourdough Bread")
}

// TestPatchCatalogSetsAndClearsShelfLife: the one field catalog_products'
// insert-only rule still permits an admin to change
// (docs/specs/02-data-model.md), sent as a JSON number or null — the same
// shape PatchCategoryShelfLife accepts, via the same parseShelfLifeDays
// helper — a non-numeric value is a 422, null clears the override back to
// "resolve per spec 08" rather than being rejected.
func TestPatchCatalogSetsAndClearsShelfLife(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := uuid.New()
	f.auth.catalog = []store.CatalogProduct{{ID: id, DisplayName: "Rice", ItemType: store.ItemLongShelfLife}}
	f.auth.catalogRecompute = map[uuid.UUID]int{id: 3}

	set := f.do(http.MethodPatch, "/api/admin/catalog/"+id.String(), `{"default_shelf_life_days":730}`)
	require.Equal(t, http.StatusOK, set.Code, set.Body.String())
	require.NotNil(t, f.auth.catalog[0].DefaultShelfLifeDays)
	assert.Equal(t, 730, *f.auth.catalog[0].DefaultShelfLifeDays)
	assert.Contains(t, set.Body.String(), `"recomputed_batches":3`,
		"the cross-storage cascade's count must reach the admin, like the storage-scoped sibling's does")
	require.Len(t, f.auth.catalogRecomputeCalls, 1)
	assert.Equal(t, id, f.auth.catalogRecomputeCalls[0], "the cascade must run against the entry that was just changed")

	cleared := f.do(http.MethodPatch, "/api/admin/catalog/"+id.String(), `{"default_shelf_life_days":null}`)
	require.Equal(t, http.StatusOK, cleared.Code)
	assert.Nil(t, f.auth.catalog[0].DefaultShelfLifeDays)
	assert.Len(t, f.auth.catalogRecomputeCalls, 2, "clearing the override is also a change the cascade must reach")

	bad := f.do(http.MethodPatch, "/api/admin/catalog/"+id.String(), `{"default_shelf_life_days":"not a number"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, bad.Code)

	missing := f.do(http.MethodPatch, "/api/admin/catalog/"+uuid.New().String(), `{"default_shelf_life_days":5}`)
	assert.Equal(t, http.StatusNotFound, missing.Code)
}

// TestDeleteCatalogRemovesEntry covers admin moderation of an insert-only
// table — the only way a bad entry is removed (docs/specs/02-data-model.md).
func TestDeleteCatalogRemovesEntry(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := uuid.New()
	f.auth.catalog = []store.CatalogProduct{{ID: id, DisplayName: "Bad Entry", ItemType: store.ItemPerishable}}

	rec := f.do(http.MethodDelete, "/api/admin/catalog/"+id.String(), "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, f.auth.catalog)

	again := f.do(http.MethodDelete, "/api/admin/catalog/"+id.String(), "")
	assert.Equal(t, http.StatusNotFound, again.Code)
}

// TestAdminPageShowsModelUnavailableBanner covers the /admin AI-model banner
// (docs/specs/01-architecture-and-deployment.md's AI model resilience): a
// warning naming the stale model, plus a picker built only from what the
// provider currently offers — the stale model itself must not appear as a
// selectable option, since selecting it again would change nothing.
func TestAdminPageShowsModelUnavailableBanner(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	f.adminVision.model = "gemini-1.0-stale"
	f.adminVision.status = "model_unavailable"
	f.adminVision.models = []string{"gemini-2.5-flash", "gemini-2.5-pro"}

	rec := f.do(http.MethodGet, "/admin", "")
	require.Equal(t, http.StatusOK, rec.Code)
	page := rec.Body.String()

	assert.Contains(t, page, "gemini-1.0-stale", "the stale model must be named so the admin knows what's broken")
	assert.Contains(t, page, "gemini-2.5-flash")
	assert.Contains(t, page, "gemini-2.5-pro")
	assert.Contains(t, page, `data-action="/api/admin/settings"`)
}

// TestAdminPageRendersEvenWhenModelsIsUnreachable: found alongside
// TestGetSettingsSurvivesAnUnreachableProvider by the E2E suite's first real
// run (issue #62) — an admin whose Gemini API key is invalid or offline
// still needs to see and use the users/storages/catalog sections of /admin;
// a provider outage in the one AI-model section must not take the whole
// page down to a 500.
func TestAdminPageRendersEvenWhenModelsIsUnreachable(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	f.adminVision.model = "gemini-2.0-flash"
	f.adminVision.status = "model_unavailable"
	f.adminVision.modelsErr = errors.New("vision: list models: connect: no route to host")

	rec := f.do(http.MethodGet, "/admin", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "<h1>Admin</h1>")
	assert.Contains(t, rec.Body.String(), f.user.Username, "the rest of the page still renders")
}

// TestAdminPageHidesBannerWhenModelIsFine: the warning is not a permanent
// fixture — it must not render at all once the effective model is one the
// provider actually offers.
func TestAdminPageHidesBannerWhenModelIsFine(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	f.adminVision.model = "gemini-2.5-flash"
	f.adminVision.status = "ok"
	f.adminVision.models = []string{"gemini-2.5-flash"}

	rec := f.do(http.MethodGet, "/admin", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "not currently offered")
}

// TestAdminAreaWorksWithNoVisionProvider is issue #61's missing coverage: a
// router built with no AdminVision at all — the deployment mode NewAdminHandler,
// admin.New and Deps.AdminVision each document as supported — must serve every
// route that reads the checker instead of panicking on the nil dependency.
// Every other fixture wires a fake, so a change that dereferenced it
// unconditionally would pass the rest of the suite.
func TestAdminAreaWorksWithNoVisionProvider(t *testing.T) {
	t.Parallel()

	noVision := func(d *httpapi.Deps) { d.AdminVision = nil }

	t.Run("settings report the model as unavailable", func(t *testing.T) {
		t.Parallel()

		f := newAdminFixture(t, noVision)
		rec := f.do(http.MethodGet, "/api/admin/settings", "")

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			GeminiModel     string   `json:"gemini_model"`
			AvailableModels []string `json:"available_models"`
			Status          string   `json:"status"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "model_unavailable", body.Status)
		assert.Empty(t, body.GeminiModel)
		assert.NotNil(t, body.AvailableModels, "an empty list, not null, like every other collection")
		assert.Empty(t, body.AvailableModels)
	})

	t.Run("saving a model still writes the override", func(t *testing.T) {
		t.Parallel()

		f := newAdminFixture(t, noVision)
		rec := f.do(http.MethodPut, "/api/admin/settings", `{"gemini_model":"gemini-2.5-flash"}`)

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		value, ok, err := f.auth.Setting(context.Background(), "gemini_model")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "gemini-2.5-flash", value)
	})

	t.Run("the page renders the unavailable banner", func(t *testing.T) {
		t.Parallel()

		f := newAdminFixture(t, noVision)
		rec := f.do(http.MethodGet, "/admin", "")

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		page := rec.Body.String()
		assert.Contains(t, page, f.user.Username, "the rest of the page renders")
		assert.Contains(t, page, "No vision model is configured.",
			"an admin page must not look healthy while every vision feature is off")
		assert.Contains(t, page, `<input name="gemini_model"`, "with no model list, the picker falls back to a text field")
	})

	t.Run("the env file falls back to the configured model", func(t *testing.T) {
		t.Parallel()

		f := newAdminFixture(t, noVision)
		rec := f.do(http.MethodGet, "/api/admin/settings/env-file", "")

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "GEMINI_MODEL=gemini-2.0-flash")
	})
}

// TestAdminPageRendersCatalogSearchResults covers the catalog moderation
// table: search results, an escaped user-typed name, and the shelf-life and
// delete forms wired to the right id.
func TestAdminPageRendersCatalogSearchResults(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := uuid.New()
	days := 30
	f.auth.catalog = []store.CatalogProduct{
		{ID: id, DisplayName: "<script>alert(1)</script>", ItemType: store.ItemPerishable, DefaultShelfLifeDays: &days},
	}

	rec := f.do(http.MethodGet, "/admin?q=script", "")
	require.Equal(t, http.StatusOK, rec.Code)
	page := rec.Body.String()

	assert.NotContains(t, page, "<script>alert(1)</script>", "user-typed catalog names must be escaped")
	assert.Contains(t, page, "&lt;script&gt;alert(1)&lt;/script&gt;")
	assert.Contains(t, page, `data-action="/api/admin/catalog/`+id.String()+`"`)
	assert.Contains(t, page, `value="30"`)
	assert.Contains(t, page, `name="q" value="script"`, "the search box must echo the query back")
}

// TestAdminPageCatalogEmptyStateNamesWhetherAQueryRanOrNot: an empty catalog
// and a search with no matches are different states worth telling apart.
func TestAdminPageCatalogEmptyStateNamesWhetherAQueryRanOrNot(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	empty := f.do(http.MethodGet, "/admin", "")
	require.Equal(t, http.StatusOK, empty.Code)
	assert.Contains(t, empty.Body.String(), "The catalog is empty.")

	f.auth.catalog = []store.CatalogProduct{{ID: uuid.New(), DisplayName: "Rice", ItemType: store.ItemLongShelfLife}}
	noMatch := f.do(http.MethodGet, "/admin?q=nonexistent", "")
	require.Equal(t, http.StatusOK, noMatch.Code)
	assert.Contains(t, noMatch.Body.String(), "No matches.")
}
