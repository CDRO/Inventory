package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/auth"
	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// The session-lifecycle half of AuthStoreFull, added to fakeAuth so the one
// fake backs both the gates and the routes that mint the sessions they check.

func (f *fakeAuth) CreateSession(_ context.Context, userID uuid.UUID, kind store.SessionKind, label *string, ttl time.Duration) (*store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token, err := store.NewToken()
	if err != nil {
		return nil, err
	}
	s := &store.Session{
		ID: token, UserID: userID, Kind: kind, Label: label,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(ttl),
	}
	f.sessions[s.ID] = s
	return s, nil
}

func (f *fakeAuth) DeleteSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
	return nil
}

func (f *fakeAuth) UserSessions(_ context.Context, userID uuid.UUID) ([]store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Session
	for _, s := range f.sessions {
		if s.UserID == userID {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeAuth) StoragesForUser(_ context.Context, userID uuid.UUID) ([]store.Storage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Storage
	for key := range f.members {
		if strings.HasSuffix(key, userID.String()) {
			id, err := uuid.Parse(strings.TrimSuffix(key, userID.String()))
			if err == nil {
				out = append(out, store.Storage{ID: id, Name: "Storage " + id.String()[:4]})
			}
		}
	}
	return out, nil
}

func (f *fakeAuth) UserByUsername(_ context.Context, username string) (*store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.Username == username {
			return u, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeAuth) CreatePairingCode(_ context.Context, userID uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code, err := store.NewToken()
	if err != nil {
		return "", err
	}
	if f.pairing == nil {
		f.pairing = map[string]uuid.UUID{}
	}
	f.pairing[code] = userID
	return code, nil
}

func (f *fakeAuth) RedeemPairingCode(_ context.Context, code string) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	userID, ok := f.pairing[code]
	if !ok {
		return uuid.Nil, store.ErrNotFound
	}
	// Single use: a redeemed code is gone.
	delete(f.pairing, code)
	return userID, nil
}

// ChangePassword mirrors store.ChangePassword: the hash is replaced and every
// session but keepSessionID goes, with "" keeping none.
//
// Done under the one mutex, which is as close as an in-memory fake gets to
// the real thing's transaction. That the two halves are genuinely atomic in
// PostgreSQL — and that an unknown user changes nothing — is proved against a
// real database in internal/store/users_test.go; what these tests prove is
// that the handlers ask for the right thing.
func (f *fakeAuth) ChangePassword(_ context.Context, userID uuid.UUID, passwordHash, keepSessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	user, ok := f.users[userID]
	if !ok {
		return store.ErrNotFound
	}
	user.PasswordHash = passwordHash

	for id, session := range f.sessions {
		if session.UserID == userID && id != keepSessionID {
			delete(f.sessions, id)
		}
	}
	return nil
}

func (f *fakeAuth) SetDisplayName(_ context.Context, userID uuid.UUID, displayName string) (*store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	user, ok := f.users[userID]
	if !ok {
		return nil, store.ErrNotFound
	}
	user.DisplayName = displayName
	return user, nil
}

// withPassword gives a fake user a real argon2id hash, so login exercises the
// actual verification rather than a stub that always agrees.
func (f *fakeAuth) withPassword(t *testing.T, u *store.User, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	require.NoError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	u.PasswordHash = hash
}

func postJSON(router http.Handler, path, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func sessionCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == httpapi.SessionCookie {
			return c
		}
	}
	return nil
}

// TestLoginSetsAHardenedSessionCookie — HttpOnly so no script reads it,
// SameSite=Lax so a cross-site form cannot ride on it, Secure outside dev.
func TestLoginSetsAHardenedSessionCookie(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	rec := postJSON(f.router, "/api/auth/login",
		`{"username":"`+f.user.Username+`","password":"correct horse"}`, nil)

	require.Equal(t, http.StatusOK, rec.Code)

	cookie := sessionCookieFrom(rec)
	require.NotNil(t, cookie, "a successful login must set the session cookie")
	assert.True(t, cookie.HttpOnly, "no script may read the session id")
	assert.True(t, cookie.Secure, "outside dev the cookie is Secure")
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.NotEmpty(t, cookie.Value)
}

// TestLoginFailuresAreIndistinguishable — a wrong password and an unknown user
// must be byte-identical, or this endpoint enumerates who has an account.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	wrongPassword := postJSON(f.router, "/api/auth/login",
		`{"username":"`+f.user.Username+`","password":"wrong"}`, nil)
	unknownUser := postJSON(f.router, "/api/auth/login",
		`{"username":"nobody-at-all","password":"wrong"}`, nil)
	emptyFields := postJSON(f.router, "/api/auth/login", `{"username":"","password":""}`, nil)

	require.Equal(t, http.StatusUnauthorized, wrongPassword.Code)
	assert.Equal(t, wrongPassword.Code, unknownUser.Code)
	assert.Equal(t, wrongPassword.Code, emptyFields.Code)
	assert.JSONEq(t, wrongPassword.Body.String(), unknownUser.Body.String(),
		"a wrong password and an unknown username must be byte-identical")
	assert.JSONEq(t, wrongPassword.Body.String(), emptyFields.Body.String())

	assert.Nil(t, sessionCookieFrom(wrongPassword), "a failed login sets no cookie")
}

// TestMeNeverCarriesIsAdmin asserts on the raw response body for an admin user,
// which is the case where a leak would matter. A client that could read
// is_admin would invite exactly the client-side branch the design forbids.
func TestMeNeverCarriesIsAdmin(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.setAdmin(f.user.ID, true)
	f.user.IsAdmin = true

	rec := f.do(http.MethodGet, "/api/auth/me", "")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "is_admin")
	assert.NotContains(t, body, "admin", "no admin signal of any kind")
	assert.NotContains(t, body, "password")
	assert.Contains(t, body, f.user.Username)
}

// TestLoginResponseAlsoOmitsIsAdmin — login returns the same shape as /me, and
// an admin logging in is the first place a leak would show up.
func TestLoginResponseAlsoOmitsIsAdmin(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.user.IsAdmin = true
	f.auth.withPassword(t, f.user, "pw")

	rec := postJSON(f.router, "/api/auth/login", `{"username":"`+f.user.Username+`","password":"pw"}`, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "is_admin")
}

// TestLogoutRevokesTheSessionImmediately — the row is deleted, so the same id
// stops working at once rather than whenever a cache notices.
func TestLogoutRevokesTheSessionImmediately(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodPost, "/api/auth/logout", "")
	require.Equal(t, http.StatusNoContent, rec.Code)

	cleared := sessionCookieFrom(rec)
	require.NotNil(t, cleared, "logout clears the cookie")
	assert.True(t, cleared.MaxAge < 0)

	after := f.do(http.MethodGet, "/api/auth/me", "")
	assert.Equal(t, http.StatusUnauthorized, after.Code, "the revoked id must not authenticate")
}

// TestBearerTransportAuthenticates — a native client cannot hold an HttpOnly
// cookie, so the same session id is accepted as a Bearer header.
func TestBearerTransportAuthenticates(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+f.session.ID)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestPairingMintsADeviceSessionInTheBody — the caller is a native client, so
// the session id comes back in the body rather than as a cookie.
func TestPairingMintsADeviceSessionInTheBody(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	codeRec := f.do(http.MethodPost, "/api/auth/pairing-codes", "")
	require.Equal(t, http.StatusCreated, codeRec.Code)

	var code struct {
		Code    string `json:"code"`
		BaseURL string `json:"base_url"`
	}
	require.NoError(t, json.Unmarshal(codeRec.Body.Bytes(), &code))
	require.NotEmpty(t, code.Code)
	assert.NotEqual(t, f.session.ID, code.Code, "the QR carries a pairing code, never a session id")

	pairRec := postJSON(f.router, "/api/auth/pair",
		`{"code":"`+code.Code+`","device_label":"Kitchen tablet"}`, nil)
	require.Equal(t, http.StatusCreated, pairRec.Code)
	assert.Nil(t, sessionCookieFrom(pairRec), "a native client gets no cookie")

	var paired struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(pairRec.Body.Bytes(), &paired))
	require.NotEmpty(t, paired.SessionID)

	// The new session works as a Bearer credential.
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+paired.SessionID)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// And the code is single use.
	again := postJSON(f.router, "/api/auth/pair", `{"code":"`+code.Code+`"}`, nil)
	assert.Equal(t, http.StatusUnauthorized, again.Code, "a redeemed code cannot mint a second session")
}

func TestAnUnknownPairingCodeIsRefused(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := postJSON(f.router, "/api/auth/pair", `{"code":"made-up"}`, nil)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Nil(t, sessionCookieFrom(rec))
}

// TestPairingIsRateLimited — the one unauthenticated endpoint that mints a
// session must not be hammerable.
func TestPairingIsRateLimited(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	var last *httptest.ResponseRecorder
	for range 15 {
		last = postJSON(f.router, "/api/auth/pair", `{"code":"guess"}`, func(r *http.Request) {
			r.RemoteAddr = "203.0.113.7:4444"
		})
	}
	assert.Equal(t, http.StatusTooManyRequests, last.Code)

	// Per address, not global: another client is unaffected.
	other := postJSON(f.router, "/api/auth/pair", `{"code":"guess"}`, func(r *http.Request) {
		r.RemoteAddr = "198.51.100.9:5555"
	})
	assert.Equal(t, http.StatusUnauthorized, other.Code)
}

// TestPairingCodeCreationIsRateLimitedPerUser — the "per user" half of spec
// 03's limit, on the endpoint where the user is known.
func TestPairingCodeCreationIsRateLimitedPerUser(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	var last *httptest.ResponseRecorder
	for range 15 {
		last = f.do(http.MethodPost, "/api/auth/pairing-codes", "")
	}
	assert.Equal(t, http.StatusTooManyRequests, last.Code)

	_, otherSession := f.auth.addUser(t, false)
	other := sendAs(f.router, otherSession.ID, http.MethodPost, "/api/auth/pairing-codes", "")
	assert.Equal(t, http.StatusCreated, other.Code, "one user's limit is not another's")
}

// TestPairLabelIsTruncatedByCharacter — a byte cut inside a multi-byte
// character would store invalid UTF-8.
func TestPairLabelIsTruncatedByCharacter(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	code, err := f.auth.CreatePairingCode(context.Background(), f.user.ID)
	require.NoError(t, err)

	label := strings.Repeat("ä", 100)
	rec := postJSON(f.router, "/api/auth/pair", `{"code":"`+code+`","device_label":"`+label+`"}`, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var body struct {
		Label string `json:"label"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, strings.Repeat("ä", 64), body.Label)
}

// TestPairingCodeURLFollowsTheForwardedScheme — Traefik terminates TLS, so
// r.TLS is nil even for an https client. An http:// URL in the QR would point a
// phone at an address it cannot reach.
func TestPairingCodeURLFollowsTheForwardedScheme(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/pairing-codes", nil)
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: f.session.ID})
	req.Host = "inventory.tailnet.ts.net"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var body struct {
		BaseURL string `json:"base_url"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "https://inventory.tailnet.ts.net", body.BaseURL)
}

type deviceItem struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Current bool   `json:"current"`
}

func listDevices(t *testing.T, f *apiFixture) (raw string, items []deviceItem) {
	t.Helper()
	rec := f.do(http.MethodGet, "/api/auth/devices", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Items []deviceItem `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec.Body.String(), body.Items
}

// TestDevicesListMarksTheCurrentSession — the UI needs "which of these is me".
func TestDevicesListMarksTheCurrentSession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	_, err := f.auth.CreateSession(context.Background(), f.user.ID, store.SessionDevice, nil, time.Hour)
	require.NoError(t, err)

	_, items := listDevices(t, f)
	require.Len(t, items, 2)

	currents := 0
	for _, d := range items {
		if d.Current {
			currents++
		}
	}
	assert.Equal(t, 1, currents, "exactly one session is the one making this request")
}

// TestDevicesListNeverCarriesASessionToken — sessions.id is the bearer token
// itself. A list that returned it would hand every live credential the user
// holds, a paired phone's year-long one included, to any script that can read
// the page. Asserted on the raw body, so no field name can smuggle one out.
func TestDevicesListNeverCarriesASessionToken(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	device, err := f.auth.CreateSession(context.Background(), f.user.ID, store.SessionDevice, nil, time.Hour)
	require.NoError(t, err)

	raw, items := listDevices(t, f)

	assert.NotContains(t, raw, f.session.ID, "the calling session's token must not be returned")
	assert.NotContains(t, raw, device.ID, "a paired device's token must not be returned")
	for _, d := range items {
		assert.NotEmpty(t, d.ID, "each device still needs a handle to revoke it by")
	}
}

// TestRevokingAnotherUsersSessionIs404 — the same non-enumeration rule as
// storages and the admin area. It must also not revoke anything, whether the
// caller names the other session by its handle or by its raw token.
func TestRevokingAnotherUsersSessionIs404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	_, other := f.auth.addUser(t, false)
	sum := sha256.Sum256([]byte(other.ID))
	otherHandle := hex.EncodeToString(sum[:])

	for _, name := range []string{otherHandle, other.ID} {
		rec := f.do(http.MethodDelete, "/api/auth/devices/"+name, "")
		require.Equal(t, http.StatusNotFound, rec.Code)
	}

	_, err := f.auth.LookupSession(context.Background(), other.ID)
	assert.NoError(t, err, "a refused revoke must not have deleted someone else's session")
}

// TestRevokingYourOwnSessionWorks goes through the list, the way a client
// has to: the handle it gets there is what revokes.
func TestRevokingYourOwnSessionWorks(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	device, err := f.auth.CreateSession(context.Background(), f.user.ID, store.SessionDevice, nil, time.Hour)
	require.NoError(t, err)

	_, items := listDevices(t, f)
	var handle string
	for _, d := range items {
		if d.Kind == string(store.SessionDevice) {
			handle = d.ID
		}
	}
	require.NotEmpty(t, handle)

	assert.Equal(t, http.StatusNotFound, f.do(http.MethodDelete, "/api/auth/devices/"+device.ID, "").Code,
		"a raw token is not a handle; accepting one would make the handle pointless")

	rec := f.do(http.MethodDelete, "/api/auth/devices/"+handle, "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, err = f.auth.LookupSession(context.Background(), device.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = f.auth.LookupSession(context.Background(), f.session.ID)
	assert.NoError(t, err, "only the named session is revoked")
}

// TestDevCookiesDropSecure — the dev carve-out. Over plain http://localhost a
// Secure cookie is never sent back, so a regression here shows up only as a
// developer's login silently not sticking.
func TestDevCookiesDropSecure(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	user, _ := auth.addUser(t, false)
	auth.withPassword(t, user, "pw-for-dev")

	router := httpapi.NewRouter(httpapi.Deps{
		DB:              stubPinger{},
		Vision:          stubVision{status: "ok"},
		Store:           newFakeAPI(auth),
		InsecureCookies: true,
	})

	rec := postJSON(router, "/api/auth/login", `{"username":"`+user.Username+`","password":"pw-for-dev"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	cookie := sessionCookieFrom(rec)
	require.NotNil(t, cookie)
	assert.False(t, cookie.Secure)
	assert.True(t, cookie.HttpOnly, "dev relaxes Secure only, never HttpOnly")
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
}

// TestAuthRoutesThatNeedASessionRequireOne — login and pair are open by
// necessity; everything else must answer 401 without a session.
func TestAuthRoutesThatNeedASessionRequireOne(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/auth/logout"},
		{http.MethodGet, "/api/auth/me"},
		{http.MethodPost, "/api/auth/pairing-codes"},
		{http.MethodGet, "/api/auth/devices"},
		{http.MethodDelete, "/api/auth/devices/whatever"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := f.anonymous(route.method, route.path, "")
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}
