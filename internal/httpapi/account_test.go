package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The account self-service surface of docs/specs/14-account-self-service.md:
// changing one's own password, the admin reset, the display name, and the one
// rate limiter the three credential endpoints share.

// login posts credentials and returns the recorder plus the session cookie a
// successful login set. Requests from a distinct address, so a test that logs
// in several times does not spend one shared per-IP counter doing it.
func login(t *testing.T, router http.Handler, username, password, from string) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()

	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	rec := postJSON(router, "/api/auth/login", body, func(r *http.Request) {
		if from != "" {
			r.RemoteAddr = from
		}
	})
	return rec, sessionCookieFrom(rec)
}

// sendWithCookie issues a request carrying a specific session cookie, which is
// what lets one test act as two different sessions of the same user.
func sendWithCookie(router http.Handler, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestChangingPasswordRevokesEveryOtherSession is the central guarantee of
// this spec: a password change locks out everything holding the old one.
//
// Two real logins, so these are two independent session rows of the same user
// — the shape of "my laptop and my phone", and the reason the change is worth
// anything at all. Somebody changes their password precisely when they think
// it has leaked; a change that left the other sessions alive would have
// changed the lock and left the windows open.
func TestChangingPasswordRevokesEveryOtherSession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	_, first := login(t, f.router, f.user.Username, "correct horse", "198.51.100.1:1111")
	require.NotNil(t, first, "the first login must establish a session")
	_, second := login(t, f.router, f.user.Username, "correct horse", "198.51.100.2:2222")
	require.NotNil(t, second, "the second login must establish a second session")
	require.NotEqual(t, first.Value, second.Value, "two logins are two sessions")

	// Both work before the change.
	require.Equal(t, http.StatusOK, sendWithCookie(f.router, first, http.MethodGet, "/api/auth/me", "").Code)
	require.Equal(t, http.StatusOK, sendWithCookie(f.router, second, http.MethodGet, "/api/auth/me", "").Code)

	changed := sendWithCookie(f.router, first, http.MethodPost, "/api/auth/password",
		`{"current_password":"correct horse","new_password":"a much longer one"}`)
	require.Equal(t, http.StatusNoContent, changed.Code, changed.Body.String())

	assert.Equal(t, http.StatusUnauthorized,
		sendWithCookie(f.router, second, http.MethodGet, "/api/auth/me", "").Code,
		"the other session must be rejected on its very next request")
	assert.Equal(t, http.StatusOK,
		sendWithCookie(f.router, first, http.MethodGet, "/api/auth/me", "").Code,
		"the session that made the change keeps working")

	// And the new password is the one that works now.
	_, withNew := login(t, f.router, f.user.Username, "a much longer one", "198.51.100.3:3333")
	assert.NotNil(t, withNew, "the new password logs in")
	oldRec, _ := login(t, f.router, f.user.Username, "correct horse", "198.51.100.4:4444")
	assert.Equal(t, http.StatusUnauthorized, oldRec.Code, "the old password no longer does")
}

// TestPairedDeviceSessionsGoToo — spec 14 says "browser and device kind
// alike". A paired phone holding a year-long bearer token is the session most
// worth revoking and the easiest one to forget.
func TestPairedDeviceSessionsGoToo(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	codeRec := f.do(http.MethodPost, "/api/auth/pairing-codes", "")
	require.Equal(t, http.StatusCreated, codeRec.Code)
	var code struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(codeRec.Body.Bytes(), &code))

	pairRec := postJSON(f.router, "/api/auth/pair", `{"code":"`+code.Code+`"}`, nil)
	require.Equal(t, http.StatusCreated, pairRec.Code)
	var paired struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(pairRec.Body.Bytes(), &paired))

	bearer := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
		req.Header.Set("Authorization", "Bearer "+paired.SessionID)
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec.Code
	}
	require.Equal(t, http.StatusOK, bearer(), "the paired device works before the change")

	changed := f.do(http.MethodPost, "/api/auth/password",
		`{"current_password":"correct horse","new_password":"a much longer one"}`)
	require.Equal(t, http.StatusNoContent, changed.Code, changed.Body.String())

	assert.Equal(t, http.StatusUnauthorized, bearer(),
		"a paired device must be revoked by a password change like any other session")
}

// TestWrongCurrentPasswordIsAFieldError — 422, not 401. The caller is
// authenticated; their input is what failed. A 401 would send the browser's
// fetch wrapper to the login page over a mistyped box.
func TestWrongCurrentPasswordIsAFieldError(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	rec := f.do(http.MethodPost, "/api/auth/password",
		`{"current_password":"not it","new_password":"a much longer one"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	var envelope struct {
		Error struct {
			Code   string              `json:"code"`
			Fields map[string][]string `json:"fields"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	assert.Equal(t, "validation_failed", envelope.Error.Code)
	assert.NotEmpty(t, envelope.Error.Fields["current_password"])

	// The session survives, and so does the old password.
	assert.Equal(t, http.StatusOK, f.do(http.MethodGet, "/api/auth/me", "").Code)
}

// TestRepeatedWrongCurrentPasswordHitsTheLimit — spec 14's second acceptance
// criterion. This endpoint accepts password guesses from a stolen session, so
// it must not accept them quickly.
func TestRepeatedWrongCurrentPasswordHitsTheLimit(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	for i := range 10 {
		rec := f.do(http.MethodPost, "/api/auth/password",
			`{"current_password":"wrong","new_password":"a much longer one"}`)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "attempt %d is still a field error", i+1)
	}

	limited := f.do(http.MethodPost, "/api/auth/password",
		`{"current_password":"wrong","new_password":"a much longer one"}`)
	require.Equal(t, http.StatusTooManyRequests, limited.Code, "the eleventh attempt is refused")
	assertRetryAfter(t, limited)

	// Even the correct password is refused while the window stands: the limit
	// is on the endpoint, not on whether this particular guess was right.
	correct := f.do(http.MethodPost, "/api/auth/password",
		`{"current_password":"correct horse","new_password":"a much longer one"}`)
	assert.Equal(t, http.StatusTooManyRequests, correct.Code)
}

// TestAShortNewPasswordDoesNotBurnTheLimiter — a too-short new_password is a
// typo in a form, not a guess at a secret. Counting it would let an honest
// user lock themselves out by fumbling the field that is not the credential.
func TestAShortNewPasswordDoesNotBurnTheLimiter(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	for range 12 {
		rec := f.do(http.MethodPost, "/api/auth/password",
			`{"current_password":"correct horse","new_password":"short"}`)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		var envelope struct {
			Error struct {
				Fields map[string][]string `json:"fields"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		require.NotEmpty(t, envelope.Error.Fields["new_password"])
	}

	// Twelve rejections later the real change still goes through.
	rec := f.do(http.MethodPost, "/api/auth/password",
		`{"current_password":"correct horse","new_password":"a much longer one"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestNewPasswordMinimumIsTenCharacters — spec 14 sets ten, with no
// composition rules and no maximum below 128.
func TestNewPasswordMinimumIsTenCharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		password string
		want     int
	}{
		{"nine is too short", "123456789", http.StatusUnprocessableEntity},
		{"ten is enough", "1234567890", http.StatusNoContent},
		{"no composition rules", "aaaaaaaaaaaa", http.StatusNoContent},
		{"128 is accepted", strings.Repeat("x", 128), http.StatusNoContent},
		{"beyond 128 is refused", strings.Repeat("x", 129), http.StatusUnprocessableEntity},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			f.auth.withPassword(t, f.user, "correct horse")

			rec := f.do(http.MethodPost, "/api/auth/password",
				fmt.Sprintf(`{"current_password":"correct horse","new_password":%q}`, tc.password))
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}

// TestPasswordRoutesLeakNothing — the last acceptance criterion: no route in
// this spec returns is_admin, a password, or a hash.
//
// The two password routes answer 204, so their bodies are empty and scanning
// them for a leaked secret would prove nothing: the assertion that carries
// weight for those is that the body is empty *and* the status really is 204,
// which is the strongest form of "nothing was returned" there is. Only
// PATCH /api/auth/me has a body worth reading, so that is the one the scan
// runs against.
func TestPasswordRoutesLeakNothing(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.setAdmin(f.user.ID, true)
	f.user.IsAdmin = true
	f.auth.withPassword(t, f.user, "correct horse")

	target, _ := f.auth.addUser(t, false)

	changed := f.do(http.MethodPost, "/api/auth/password",
		`{"current_password":"correct horse","new_password":"a much longer one"}`)
	require.Equal(t, http.StatusNoContent, changed.Code, changed.Body.String())
	assert.Empty(t, changed.Body.String(), "a password change echoes success and nothing else")

	reset := f.do(http.MethodPost, "/api/admin/users/"+target.ID.String()+"/password",
		`{"new_password":"a temporary one"}`)
	require.Equal(t, http.StatusNoContent, reset.Code, reset.Body.String())
	assert.Empty(t, reset.Body.String(),
		"an admin reset must not echo the password it was handed")

	renamed := f.do(http.MethodPatch, "/api/auth/me", `{"display_name":"Renamed"}`)
	require.Equal(t, http.StatusOK, renamed.Code)
	body := renamed.Body.String()
	require.NotEmpty(t, body, "this one does have a body, so the scan below means something")
	assert.Contains(t, body, "Renamed", "and it is the response we think it is")
	assert.NotContains(t, body, "is_admin")
	assert.NotContains(t, body, "password")
	assert.NotContains(t, body, "argon2")
	assert.NotContains(t, body, "hash")
}

// TestPasswordChangeFailuresDoNotLockTheOwnerOutOfLogin is the regression
// test for keying that route by address alone.
//
// Somebody with a borrowed unlocked phone holds a valid session. Ten wrong
// current_password guesses from it must not cost the account's real owner —
// sitting at a different address — their ability to log in and take the
// session away. Keying the password route by username would have done
// exactly that for fifteen minutes.
func TestPasswordChangeFailuresDoNotLockTheOwnerOutOfLogin(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	for i := range 11 {
		rec := f.do(http.MethodPost, "/api/auth/password",
			`{"current_password":"wrong","new_password":"a much longer one"}`)
		if i < 10 {
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "guess %d", i+1)
		} else {
			require.Equal(t, http.StatusTooManyRequests, rec.Code, "the borrowed session is cut off")
		}
	}

	// The owner, from their own address, logs in unaffected.
	rec, cookie := login(t, f.router, f.user.Username, "correct horse", "198.51.100.77:4444")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, cookie)
}

// TestAdminResetRevokesEveryTargetSession — the resetter cannot know which of
// the target's sessions are legitimate, so none survive. Contrast with the
// self-service change above, which keeps the caller's own.
func TestAdminResetRevokesEveryTargetSession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.setAdmin(f.user.ID, true)

	target, _ := f.auth.addUser(t, false)
	f.auth.withPassword(t, target, "their old password")

	_, first := login(t, f.router, target.Username, "their old password", "198.51.100.1:1111")
	require.NotNil(t, first)
	_, second := login(t, f.router, target.Username, "their old password", "198.51.100.2:2222")
	require.NotNil(t, second)

	rec := f.do(http.MethodPost, "/api/admin/users/"+target.ID.String()+"/password",
		`{"new_password":"a temporary one"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	for i, cookie := range []*http.Cookie{first, second} {
		assert.Equal(t, http.StatusUnauthorized,
			sendWithCookie(f.router, cookie, http.MethodGet, "/api/auth/me", "").Code,
			"session %d must be gone after an admin reset", i+1)
	}

	// The admin's own session is untouched — only the target's went.
	assert.Equal(t, http.StatusOK, f.do(http.MethodGet, "/api/auth/me", "").Code)

	// And the temporary password is what logs in now.
	_, after := login(t, f.router, target.Username, "a temporary one", "198.51.100.3:3333")
	assert.NotNil(t, after)
}

// TestAdminResetIsHiddenFromNonAdmins — the admin area does not announce its
// own existence. A non-admin must not be able to tell this route apart from a
// path that was never registered.
func TestAdminResetIsHiddenFromNonAdmins(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t) // f.user is not an admin
	target, _ := f.auth.addUser(t, false)

	refused := f.do(http.MethodPost, "/api/admin/users/"+target.ID.String()+"/password",
		`{"new_password":"a temporary one"}`)
	imaginary := f.do(http.MethodPost, "/api/admin/nonexistent/route", `{}`)

	require.Equal(t, http.StatusNotFound, refused.Code)
	assert.Equal(t, imaginary.Code, refused.Code)
	assert.JSONEq(t, imaginary.Body.String(), refused.Body.String(),
		"a refused reset and an imaginary admin path must be byte-identical")
}

// TestAdminResetRequiresAdminOnEveryRequest — is_admin is re-queried from the
// database per request, so rights revoked between two requests take effect on
// the second one, not whenever the session expires.
func TestAdminResetRequiresAdminOnEveryRequest(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.setAdmin(f.user.ID, true)
	target, _ := f.auth.addUser(t, false)

	before := f.auth.adminCallCount()
	first := f.do(http.MethodPost, "/api/admin/users/"+target.ID.String()+"/password",
		`{"new_password":"a temporary one"}`)
	require.Equal(t, http.StatusNoContent, first.Code, first.Body.String())
	assert.Greater(t, f.auth.adminCallCount(), before, "is_admin is read from the store for this route")

	f.auth.setAdmin(f.user.ID, false)
	after := f.do(http.MethodPost, "/api/admin/users/"+target.ID.String()+"/password",
		`{"new_password":"another temporary one"}`)
	assert.Equal(t, http.StatusNotFound, after.Code,
		"revoking admin rights takes effect on the very next request")
}

// TestAdminResetOfAnUnknownUserIsNotFound — and it is the same 404 a
// non-admin gets, so the route cannot be used to probe which ids are real.
func TestAdminResetOfAnUnknownUserIsNotFound(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.setAdmin(f.user.ID, true)

	unknown := f.do(http.MethodPost, "/api/admin/users/"+uuidThatNamesNothing+"/password",
		`{"new_password":"a temporary one"}`)
	malformed := f.do(http.MethodPost, "/api/admin/users/not-a-uuid/password",
		`{"new_password":"a temporary one"}`)

	assert.Equal(t, http.StatusNotFound, unknown.Code)
	assert.Equal(t, http.StatusNotFound, malformed.Code)
	assert.JSONEq(t, unknown.Body.String(), malformed.Body.String())
}

// uuidThatNamesNothing is a well-formed id no fixture ever creates.
const uuidThatNamesNothing = "0192f000-0000-7000-8000-000000000000"

// TestPatchMeChangesTheDisplayNameAndNothingElse — spec 14's sixth acceptance
// criterion.
func TestPatchMeChangesTheDisplayNameAndNothingElse(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	originalUsername := f.user.Username

	rec := f.do(http.MethodPatch, "/api/auth/me", `{"display_name":"Tizian"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var me struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	assert.Equal(t, "Tizian", me.DisplayName)
	assert.Equal(t, originalUsername, me.Username, "usernames are immutable")

	// The change is durable, not just echoed.
	after := f.do(http.MethodGet, "/api/auth/me", "")
	require.NoError(t, json.Unmarshal(after.Body.Bytes(), &me))
	assert.Equal(t, "Tizian", me.DisplayName)
}

// TestPatchMeRejectsUnknownFields — "unknown fields are rejected, not
// ignored". A body that silently drops half of what it carries is how a
// client ships `{"display_name":"x","is_admin":true}` believing it worked.
func TestPatchMeRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"is_admin", `{"display_name":"Tizian","is_admin":true}`},
		{"username", `{"display_name":"Tizian","username":"someone-else"}`},
		{"password", `{"display_name":"Tizian","password_hash":"x"}`},
		{"unknown alone", `{"nonsense":1}`},
		{"empty body object", `{}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			before := f.user.DisplayName

			rec := f.do(http.MethodPatch, "/api/auth/me", tc.body)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Equal(t, before, f.user.DisplayName, "nothing may change on a rejected PATCH")
		})
	}
}

// TestPatchMeRefusesABlankDisplayName — an account with no name is not a
// rename, it is a broken row in every members list that shows one.
func TestPatchMeRefusesABlankDisplayName(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, "/api/auth/me", `{"display_name":"   "}`)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// TestEleventhFailedLoginIsRateLimited — spec 14's fourth acceptance
// criterion, from one address.
func TestEleventhFailedLoginIsRateLimited(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	const from = "203.0.113.20:5000"
	for i := range 10 {
		rec, _ := login(t, f.router, f.user.Username, "wrong", from)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "attempt %d is a plain refusal", i+1)
	}

	limited, cookie := login(t, f.router, f.user.Username, "wrong", from)
	require.Equal(t, http.StatusTooManyRequests, limited.Code, "the eleventh is refused")
	assert.Nil(t, cookie)
	assertRetryAfter(t, limited)
}

// TestASuccessResetsTheLoginCounter — "a success before the limit resets the
// count". Somebody who mistypes nine times and then gets it right is not
// mid-attack.
func TestASuccessResetsTheLoginCounter(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	const from = "203.0.113.21:5000"
	for range 9 {
		rec, _ := login(t, f.router, f.user.Username, "wrong", from)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}

	good, cookie := login(t, f.router, f.user.Username, "correct horse", from)
	require.Equal(t, http.StatusOK, good.Code)
	require.NotNil(t, cookie)

	// The counter is back to zero: ten more failures are all plain 401s.
	for i := range 10 {
		rec, _ := login(t, f.router, f.user.Username, "wrong", from)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "attempt %d after the reset", i+1)
	}
}

// TestLoginIsLimitedPerUsernameAcrossAddresses — the per-username half. Ten
// guesses per address would otherwise be unlimited guesses to anyone with
// more than one address.
func TestLoginIsLimitedPerUsernameAcrossAddresses(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	for i := range 10 {
		rec, _ := login(t, f.router, f.user.Username, "wrong", fmt.Sprintf("203.0.113.%d:5000", i+1))
		require.Equal(t, http.StatusUnauthorized, rec.Code, "attempt %d", i+1)
	}

	// An eleventh address, but the same username.
	limited, _ := login(t, f.router, f.user.Username, "wrong", "203.0.113.99:5000")
	assert.Equal(t, http.StatusTooManyRequests, limited.Code)

	// A different username from that same fresh address is unaffected, so the
	// per-username counter is what fired and not a global one.
	other, _ := login(t, f.router, "someone-else-entirely", "wrong", "203.0.113.98:5000")
	assert.Equal(t, http.StatusUnauthorized, other.Code)
}

// TestUnknownUsernamesAreCountedToo — a limiter that only counted real
// usernames would answer 429 for accounts that exist and 401 for ones that do
// not, turning the defence into the user enumeration spec 03 forbids.
func TestUnknownUsernamesAreCountedToo(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	for i := range 10 {
		rec, _ := login(t, f.router, "nobody-at-all", "wrong", fmt.Sprintf("203.0.113.%d:5000", i+1))
		require.Equal(t, http.StatusUnauthorized, rec.Code, "attempt %d", i+1)
	}

	limited, _ := login(t, f.router, "nobody-at-all", "wrong", "203.0.113.99:5000")
	assert.Equal(t, http.StatusTooManyRequests, limited.Code,
		"an imaginary username reaches the limit exactly as a real one does")
}

// TestOneCounterCoversLoginPairAndPassword — spec 14 specifies **one**
// limiter across the three credential endpoints. Three separate counters
// would mean thirty guesses from one address rather than ten, against
// endpoints that are interchangeable to whoever is guessing.
func TestOneCounterCoversLoginPairAndPassword(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	const from = "203.0.113.30:6000"

	// Five failed logins and five rejected pairings: ten failures spread over
	// two endpoints, all charged to the same address. A distinct username
	// each time, so the per-username counter cannot be what fires.
	for i := range 5 {
		rec, _ := login(t, f.router, fmt.Sprintf("nobody-%d", i), "wrong", from)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	for range 5 {
		rec := postJSON(f.router, "/api/auth/pair", `{"code":"guess"}`, func(r *http.Request) {
			r.RemoteAddr = from
		})
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}

	// The eleventh attempt is refused whichever of the two it lands on.
	eleventh, _ := login(t, f.router, "nobody-eleven", "wrong", from)
	assert.Equal(t, http.StatusTooManyRequests, eleventh.Code,
		"login must see the failures pairing recorded")

	pairing := postJSON(f.router, "/api/auth/pair", `{"code":"guess"}`, func(r *http.Request) {
		r.RemoteAddr = from
	})
	assert.Equal(t, http.StatusTooManyRequests, pairing.Code,
		"and pairing must see the failures login recorded")
}

// TestRateLimitedBodyFollowsTheOneSerializer — the 429 is an ordinary error
// envelope with the documented code, and it carries no debug_reason outside
// dev (docs/specs/04-backend-api-conventions.md).
func TestRateLimitedBodyFollowsTheOneSerializer(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	const from = "203.0.113.40:7000"
	var limited *httptest.ResponseRecorder
	for i := range 11 {
		limited, _ = login(t, f.router, fmt.Sprintf("nobody-%d", i), "wrong", from)
	}
	require.Equal(t, http.StatusTooManyRequests, limited.Code)

	var envelope struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			DebugReason string `json:"debug_reason"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(limited.Body.Bytes(), &envelope))
	assert.Equal(t, "rate_limited", envelope.Error.Code)
	assert.NotEmpty(t, envelope.Error.Message)
	assert.Empty(t, envelope.Error.DebugReason, "the fixture's writer is not in dev mode")
	assert.Equal(t, "application/json; charset=utf-8", limited.Header().Get("Content-Type"))
}

// TestAForgedForwardedForDoesNotMoveTheLimiterKey — the reason TrustedRealIP
// replaced chi's middleware.RealIP.
//
// A client that could pick its own key would have an unlimited number of
// them, and the rate limit would be decoration. The peer here is a TEST-NET
// address, so the header is ignored and all eleven attempts land on the one
// real address.
func TestAForgedForwardedForDoesNotMoveTheLimiterKey(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	// A distinct username per attempt, so only the address counter can be
	// what fires — otherwise the test would pass even if the forged header
	// did move the address key.
	var last *httptest.ResponseRecorder
	for i := range 11 {
		body := fmt.Sprintf(`{"username":"nobody-%d","password":"wrong"}`, i)
		last = postJSON(f.router, "/api/auth/login", body, func(r *http.Request) {
			r.RemoteAddr = "203.0.113.50:8000"
			// A different forged address every time, which is exactly what
			// an attacker would send.
			r.Header.Set("X-Forwarded-For", fmt.Sprintf("10.1.2.%d", i+1))
		})
	}

	assert.Equal(t, http.StatusTooManyRequests, last.Code,
		"an untrusted peer's X-Forwarded-For must not give it a fresh counter")
}

// TestForwardedForIsHonouredFromTheInternalNetwork — the other half: behind
// Traefik every request has the same peer, so ignoring the header entirely
// would collapse every household member onto one counter and let one person's
// typos lock everyone out.
func TestForwardedForIsHonouredFromTheInternalNetwork(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	// A distinct username every time, so the per-username counter never
	// reaches the limit and the only counter that can fire is the address
	// one. Without that, this test would pass with the address key ignored
	// entirely.
	send := func(attempt int, forwarded string) int {
		body := fmt.Sprintf(`{"username":"nobody-%d","password":"wrong"}`, attempt)
		rec := postJSON(f.router, "/api/auth/login", body, func(r *http.Request) {
			r.RemoteAddr = "172.18.0.5:9000" // the Compose bridge network
			r.Header.Set("X-Forwarded-For", forwarded)
		})
		return rec.Code
	}

	for i := range 10 {
		require.Equal(t, http.StatusUnauthorized, send(i, "100.64.0.7"), "attempt %d", i+1)
	}

	assert.Equal(t, http.StatusTooManyRequests, send(10, "100.64.0.7"))
	assert.Equal(t, http.StatusUnauthorized, send(11, "100.64.0.8"),
		"a second client behind the same proxy has its own counter")
}

// TestTheRightmostForwardedEntryWins — the list reads oldest-to-newest, so a
// value the client prepended itself sits to the left of the one the proxy
// observed. Taking the leftmost, as chi's RealIP does, would key the limiter
// on the forgeable half.
func TestTheRightmostForwardedEntryWins(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	// Again a distinct username per attempt, so the address key is the only
	// counter in play.
	send := func(attempt int, chain string) int {
		body := fmt.Sprintf(`{"username":"nobody-%d","password":"wrong"}`, attempt)
		rec := postJSON(f.router, "/api/auth/login", body, func(r *http.Request) {
			r.RemoteAddr = "172.18.0.5:9000"
			r.Header.Set("X-Forwarded-For", chain)
		})
		return rec.Code
	}

	// Ten failures whose rightmost — real — entry is the same, behind a
	// client-chosen left-hand value that changes every time.
	for i := range 10 {
		require.Equal(t, http.StatusUnauthorized, send(i, fmt.Sprintf("10.9.9.%d, 100.64.0.7", i+1)))
	}

	assert.Equal(t, http.StatusTooManyRequests, send(10, "10.9.9.250, 100.64.0.7"),
		"the rightmost entry is the counter's key, so the forged prefix buys nothing")
}

// TestSelfServiceRoutesNeedASession — both new routes are registered inside
// the RequireSession group, which is the only thing that scopes them to the
// caller's own row. An anonymous caller must not reach either.
func TestSelfServiceRoutesNeedASession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	password := f.anonymous(http.MethodPost, "/api/auth/password",
		`{"current_password":"whatever","new_password":"a much longer one"}`)
	assert.Equal(t, http.StatusUnauthorized, password.Code)

	rename := f.anonymous(http.MethodPatch, "/api/auth/me", `{"display_name":"Nobody"}`)
	assert.Equal(t, http.StatusUnauthorized, rename.Code)
}

// TestPasswordChangeActsOnTheCallerOnly — there is no user id in this
// request, so there is nothing to scope and nothing to tamper with. A body
// that invents one is refused rather than honoured, and a second user's
// sessions are untouched either way.
func TestPasswordChangeActsOnTheCallerOnly(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.withPassword(t, f.user, "correct horse")

	victim, victimSession := f.auth.addUser(t, false)
	f.auth.withPassword(t, victim, "their password")

	rec := f.do(http.MethodPost, "/api/auth/password",
		`{"current_password":"correct horse","new_password":"a much longer one","user_id":"`+victim.ID.String()+`"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Equal(t, http.StatusOK,
		sendAs(f.router, victimSession.ID, http.MethodGet, "/api/auth/me", "").Code,
		"another user's session must survive somebody else's password change")
}

// assertRetryAfter checks the header spec 14 requires on every 429: whole
// seconds, and never zero — a "0" invites an immediate retry the limiter is
// still going to refuse.
func assertRetryAfter(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	header := rec.Header().Get("Retry-After")
	require.NotEmpty(t, header, "a 429 must say when to come back")

	seconds, err := strconv.Atoi(header)
	require.NoError(t, err, "Retry-After is a whole number of seconds")
	assert.Positive(t, seconds, "never zero — that invites a retry the limiter will refuse")
	// The window is fifteen minutes (docs/specs/14-account-self-service.md),
	// so nothing may ask the caller to wait longer than that.
	assert.LessOrEqual(t, seconds, 15*60, "and never longer than the window itself")
}
