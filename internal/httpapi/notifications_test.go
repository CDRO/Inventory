package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/notify"
	"github.com/CDRO/Inventory/internal/store"
)

// fakeNotifications stands in for the notification half of the store.
type fakeNotifications struct {
	settings  *store.NotificationSettings
	loadErr   error
	saveErr   error
	saved     store.NotificationSettingsInput
	savedFor  uuid.UUID
	saveCalls int
}

func (f *fakeNotifications) NotificationSettingsFor(_ context.Context, _ uuid.UUID) (*store.NotificationSettings, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if f.settings == nil {
		return nil, store.ErrNotFound
	}
	copied := *f.settings
	return &copied, nil
}

func (f *fakeNotifications) SaveNotificationSettings(_ context.Context, storageID uuid.UUID, in store.NotificationSettingsInput) (*store.NotificationSettings, error) {
	f.saveCalls++
	f.savedFor = storageID
	f.saved = in
	if f.saveErr != nil {
		return nil, f.saveErr
	}
	stored := store.NotificationSettings{
		StorageID: storageID, Enabled: in.Enabled, Kind: in.Kind, URL: in.URL,
		SendHour: in.SendHour, IncludeSoon: in.IncludeSoon,
	}
	switch {
	case in.Token != nil:
		stored.Token = *in.Token
	case f.settings != nil:
		stored.Token = f.settings.Token
	}
	f.settings = &stored
	return &stored, nil
}

// fakeNotifier stands in for the delivery service.
type fakeNotifier struct {
	err    error
	calls  int
	lastID uuid.UUID
}

func (f *fakeNotifier) SendTest(_ context.Context, settings store.NotificationSettings) error {
	f.calls++
	f.lastID = settings.StorageID
	return f.err
}

func configured(storageID uuid.UUID) *store.NotificationSettings {
	ran := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	result := store.NotificationSent
	return &store.NotificationSettings{
		StorageID: storageID, Enabled: true, Kind: store.NotificationNtfy,
		URL: "https://ntfy.example/inventory", Token: "tk_super_secret",
		SendHour: 8, IncludeSoon: true, LastRunAt: &ran, LastResult: &result,
	}
}

func TestGetNotificationSettingsDefaultsWhenNeverConfigured(t *testing.T) {
	f := newAPIFixture(t)

	rec := f.do(http.MethodGet, f.base()+"/notification-settings", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, false, body["enabled"], "a storage nobody configured is not notified")
	require.Equal(t, false, body["has_token"])
	require.Equal(t, float64(8), body["send_hour"])
}

// The spec's flat rule: a stored token is never returned by any read. The
// assertion is on the raw bytes as well as the decoded object, because a
// response can carry a field no struct in this test mentions.
func TestGetNotificationSettingsNeverReturnsTheToken(t *testing.T) {
	f := newAPIFixture(t)
	f.notifications.settings = configured(f.storageID)

	rec := f.do(http.MethodGet, f.base()+"/notification-settings", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "tk_super_secret")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotContains(t, body, "token", "there is no token field at all, not even an empty one")
	require.Equal(t, true, body["has_token"], "the UI still learns that one is stored")
}

// last_result is written by the delivery path and read back here. A summary
// built from a transport error would carry the request URL — and a gotify URL
// carries the token in its query string — so this checks the read end of that
// rule as well as the write end.
func TestGetNotificationSettingsLastResultCarriesNoToken(t *testing.T) {
	f := newAPIFixture(t)
	settings := configured(f.storageID)
	summary := "the target answered HTTP 401"
	settings.LastResult = &summary
	f.notifications.settings = settings

	rec := f.do(http.MethodGet, f.base()+"/notification-settings", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "tk_super_secret")
	require.Contains(t, rec.Body.String(), "the target answered HTTP 401")
}

func TestPutNotificationSettingsStoresTheConfiguration(t *testing.T) {
	f := newAPIFixture(t)

	rec := f.do(http.MethodPut, f.base()+"/notification-settings", `{
		"enabled": true, "kind": "gotify", "url": "http://gotify.lan",
		"token": "tk_new", "send_hour": 19, "include_soon": true
	}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, f.storageID, f.notifications.savedFor, "scoped to the storage in the path")
	require.Equal(t, "gotify", f.notifications.saved.Kind)
	require.Equal(t, 19, f.notifications.saved.SendHour)
	require.NotNil(t, f.notifications.saved.Token)
	require.Equal(t, "tk_new", *f.notifications.saved.Token)
	require.NotContains(t, rec.Body.String(), "tk_new", "not even the value just sent comes back")
}

// Absent, null and present are three different requests. Collapsing the first
// two would make a form that never sees the token erase it on every save.
func TestPutNotificationSettingsTokenIsThreeStated(t *testing.T) {
	t.Run("absent keeps the stored token", func(t *testing.T) {
		f := newAPIFixture(t)
		rec := f.do(http.MethodPut, f.base()+"/notification-settings",
			`{"enabled": true, "kind": "ntfy", "url": "https://ntfy.example/x", "send_hour": 8}`)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Nil(t, f.notifications.saved.Token)
	})

	t.Run("null clears it", func(t *testing.T) {
		f := newAPIFixture(t)
		rec := f.do(http.MethodPut, f.base()+"/notification-settings",
			`{"enabled": true, "kind": "ntfy", "url": "https://ntfy.example/x", "token": null, "send_hour": 8}`)
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotNil(t, f.notifications.saved.Token)
		require.Equal(t, "", *f.notifications.saved.Token)
	})
}

func TestPutNotificationSettingsRejectsUnusableTargets(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"a non-http scheme", `{"kind":"ntfy","url":"file:///etc/passwd","send_hour":8}`, "url"},
		{"no host at all", `{"kind":"ntfy","url":"notaurl","send_hour":8}`, "url"},
		{"an unknown kind", `{"kind":"telegram","url":"https://ntfy.example/x","send_hour":8}`, "kind"},
		{"an hour outside the day", `{"kind":"ntfy","url":"https://ntfy.example/x","send_hour":24}`, "send_hour"},
		{"a negative hour", `{"kind":"ntfy","url":"https://ntfy.example/x","send_hour":-1}`, "send_hour"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			rec := f.do(http.MethodPut, f.base()+"/notification-settings", tc.body)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			require.Equal(t, "validation_failed", errorCode(t, rec))
			require.Zero(t, f.notifications.saveCalls, "a refused save writes nothing")

			var body struct {
				Error struct {
					Fields map[string][]string `json:"fields"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Contains(t, body.Error.Fields, tc.field)
		})
	}
}

func TestNotificationTestSendsAndReportsInline(t *testing.T) {
	f := newAPIFixture(t)
	f.notifications.settings = configured(f.storageID)

	rec := f.do(http.MethodPost, f.base()+"/notification-settings/test", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, f.notifier.calls)
	require.Equal(t, f.storageID, f.notifier.lastID, "the test uses this storage's own configuration")

	var body struct {
		OK     bool   `json:"ok"`
		Result string `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.True(t, body.OK)
	require.Equal(t, store.NotificationSent, body.Result)
}

// A delivery that fails is still a request that succeeded: the person gets the
// reason next to the button instead of a generic error envelope. The reason
// itself comes from the delivery service's curated summary, never from a
// transport error string.
func TestNotificationTestReportsAFailureInline(t *testing.T) {
	f := newAPIFixture(t)
	f.notifications.settings = configured(f.storageID)
	f.notifier.err = notify.NewDeliveryError("the target answered HTTP 404")

	rec := f.do(http.MethodPost, f.base()+"/notification-settings/test", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		OK     bool   `json:"ok"`
		Result string `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.False(t, body.OK)
	require.Equal(t, "the target answered HTTP 404", body.Result)
	require.NotContains(t, rec.Body.String(), "tk_super_secret")
}

// "With enabled = FALSE ... no code path can send" — the test button is a
// code path.
func TestNotificationTestRefusesWhileDisabled(t *testing.T) {
	f := newAPIFixture(t)
	settings := configured(f.storageID)
	settings.Enabled = false
	f.notifications.settings = settings

	rec := f.do(http.MethodPost, f.base()+"/notification-settings/test", "")
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Zero(t, f.notifier.calls, "nothing left the process")
}

func TestNotificationTestIsAbsentWithoutADeliveryService(t *testing.T) {
	f := newAPIFixture(t, func(d *httpapi.Deps) { d.Notifier = nil })
	f.notifications.settings = configured(f.storageID)

	rec := f.do(http.MethodPost, f.base()+"/notification-settings/test", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	// Reading and saving still work without one.
	require.Equal(t, http.StatusOK, f.do(http.MethodGet, f.base()+"/notification-settings", "").Code)
}

// The storage-scoping rule, on all three routes at once: a storage that does
// not exist and one the caller cannot reach are the same 404, and neither is
// ever a 403 (docs/specs/03-auth-and-multi-tenancy.md).
func TestNotificationRoutesAreStorageScoped(t *testing.T) {
	f := newAPIFixture(t)
	f.notifications.settings = configured(f.storageID)

	otherStorage := uuid.New()
	foreign := "/api/storages/" + otherStorage.String() + "/notification-settings"
	unknown := "/api/storages/" + uuid.New().String() + "/notification-settings"

	for _, req := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, foreign, ""},
		{http.MethodGet, unknown, ""},
		{http.MethodPut, foreign, `{"kind":"ntfy","url":"https://ntfy.example/x","send_hour":8}`},
		{http.MethodPut, unknown, `{"kind":"ntfy","url":"https://ntfy.example/x","send_hour":8}`},
		{http.MethodPost, foreign + "/test", ""},
		{http.MethodPost, unknown + "/test", ""},
	} {
		rec := f.do(req.method, req.path, req.body)
		require.Equal(t, http.StatusNotFound, rec.Code, "%s %s", req.method, req.path)
		require.Equal(t, "not_found", errorCode(t, rec))
		require.False(t, strings.Contains(rec.Body.String(), "forbidden"))
	}
	require.Zero(t, f.notifications.saveCalls)
	require.Zero(t, f.notifier.calls)
}
