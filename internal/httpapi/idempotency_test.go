package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

type idemRecord struct {
	hash      string
	status    int
	body      []byte
	storageID *uuid.UUID
}

// fakeIdempotency mirrors the store's contract: keyed by (key, user), a hash
// mismatch is ErrValidation, and the first record for a key wins.
type fakeIdempotency struct {
	mu      sync.Mutex
	records map[string]idemRecord
}

func newFakeIdempotency() *fakeIdempotency {
	return &fakeIdempotency{records: map[string]idemRecord{}}
}

func (f *fakeIdempotency) LookupIdempotent(_ context.Context, key string, userID uuid.UUID, hash string) (*store.IdempotentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[key+userID.String()]
	if !ok {
		return nil, nil
	}
	if rec.hash != hash {
		return nil, fmt.Errorf("%w: idempotency key reused for a different request", store.ErrValidation)
	}
	return &store.IdempotentResponse{Status: rec.status, Body: rec.body}, nil
}

func (f *fakeIdempotency) RecordIdempotent(_ context.Context, key string, userID uuid.UUID, storageID *uuid.UUID, hash string, status int, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.records[key+userID.String()]; !exists {
		f.records[key+userID.String()] = idemRecord{hash: hash, status: status, body: append([]byte(nil), body...), storageID: storageID}
	}
	return nil
}

func (f *fakeIdempotency) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

func (f *fakeIdempotency) get(key string, userID uuid.UUID) (idemRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[key+userID.String()]
	return rec, ok
}

// doKeyed issues a request as the fixture's member, with an Idempotency-Key.
func (f *apiFixture) doKeyed(method, path, body, key string) *httptest.ResponseRecorder {
	return f.doKeyedAs(f.session.ID, method, path, body, key)
}

func (f *apiFixture) doKeyedAs(sessionID, method, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: sessionID})
	if key != "" {
		req.Header.Set(httpapi.IdempotencyKeyHeader, key)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// TestARetriedWriteRunsOnce is the reason the middleware exists: the retry of a
// request whose response was lost gets the original answer, and the write is
// applied once.
func TestARetriedWriteRunsOnce(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	key := uuid.NewString()
	body := `{"name":"Pantry"}`

	first := f.doKeyed(http.MethodPost, f.base()+"/locations", body, key)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())

	retry := f.doKeyed(http.MethodPost, f.base()+"/locations", body, key)
	require.Equal(t, http.StatusCreated, retry.Code)

	assert.Equal(t, int32(1), f.locations.creates.Load(), "the retry must not create a second location")
	assert.JSONEq(t, first.Body.String(), retry.Body.String(), "the retry gets the original answer, including its id")
	assert.Equal(t, "true", retry.Header().Get(httpapi.IdempotentReplayHeader))
	assert.Empty(t, first.Header().Get(httpapi.IdempotentReplayHeader))
	assert.Equal(t, "application/json; charset=utf-8", retry.Header().Get("Content-Type"))

	rec, ok := f.idem.get(key, f.user.ID)
	require.True(t, ok)
	require.NotNil(t, rec.storageID)
	assert.Equal(t, f.storageID, *rec.storageID, "the record carries the storage it was made in")
}

// TestWithoutAKeyWritesAreNotDeduplicated — the browser sends no key, and two
// clicks are two writes.
func TestWithoutAKeyWritesAreNotDeduplicated(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"A"}`, "")
	f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"A"}`, "")

	assert.Equal(t, int32(2), f.locations.creates.Load())
	assert.Zero(t, f.idem.count())
}

// TestAReusedKeyForADifferentRequestIs422 — replaying here would answer a
// different request than the one sent and silently drop it.
func TestAReusedKeyForADifferentRequestIs422(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	key := uuid.NewString()

	require.Equal(t, http.StatusCreated, f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, key).Code)

	for _, tc := range []struct{ name, method, path, body string }{
		{"different body", http.MethodPost, f.base() + "/locations", `{"name":"Cellar"}`},
		{"different path", http.MethodPost, f.base() + "/locations?x=1", `{"name":"Pantry"}`},
	} {
		rec := f.doKeyed(tc.method, tc.path, tc.body, key)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, tc.name)
		assert.Contains(t, rec.Body.String(), httpapi.IdempotencyKeyHeader, tc.name)
	}
	assert.Equal(t, int32(1), f.locations.creates.Load(), "neither mismatch may execute")
}

func TestAKeyMustBeAUUID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, "retry-1")

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Zero(t, f.locations.creates.Load(), "a rejected key must not let the write through unprotected")
}

// TestKeysAreScopedPerUser — two members who happen to send the same key are
// two different requests.
func TestKeysAreScopedPerUser(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	other, otherSession := f.auth.addUser(t, false)
	f.auth.addMember(f.storageID, other.ID)
	key := uuid.NewString()

	require.Equal(t, http.StatusCreated, f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, key).Code)
	rec := f.doKeyedAs(otherSession.ID, http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, key)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Empty(t, rec.Header().Get(httpapi.IdempotentReplayHeader), "another user's key is not this user's record")
	assert.Equal(t, int32(2), f.locations.creates.Load())
}

// TestServerErrorsAreNotRecorded — a 5xx means "try again". Recording it would
// make every retry the client was told to make return the same failure.
func TestServerErrorsAreNotRecorded(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	key := uuid.NewString()

	f.locations.createErr = errors.New("database fell over")
	require.Equal(t, http.StatusInternalServerError, f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, key).Code)
	assert.Zero(t, f.idem.count())

	f.locations.createErr = nil
	rec := f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, key)
	assert.Equal(t, http.StatusCreated, rec.Code, "the retry runs for real")
	assert.Equal(t, int32(2), f.locations.creates.Load())
}

// TestClientErrorsAreRecorded — a 409 is the request's real outcome, so a
// retry gets the same 409 back rather than a second attempt that might now
// succeed and apply a write the client already saw rejected.
func TestClientErrorsAreRecorded(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	key := uuid.NewString()

	f.locations.createErr = store.ErrConflict
	first := f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, key)
	require.Equal(t, http.StatusConflict, first.Code)

	f.locations.createErr = nil
	retry := f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, key)
	assert.Equal(t, http.StatusConflict, retry.Code)
	assert.JSONEq(t, first.Body.String(), retry.Body.String())
	assert.Equal(t, int32(1), f.locations.creates.Load())
}

// TestConcurrentCopiesOfAKeyExecuteOnce — an offline queue flushing on
// reconnect can fire the same request twice at once. Both must converge on one
// write.
func TestConcurrentCopiesOfAKeyExecuteOnce(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	key := uuid.NewString()

	const copies = 10
	results := make([]*httptest.ResponseRecorder, copies)
	var wg sync.WaitGroup
	for i := range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = f.doKeyed(http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, key)
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), f.locations.creates.Load())
	for _, rec := range results {
		assert.Equal(t, http.StatusCreated, rec.Code)
		assert.JSONEq(t, results[0].Body.String(), rec.Body.String())
	}
}

// TestARefusedRequestIsNeverRecorded — the gates run first. A non-member's
// keyed write is the ordinary 404, and leaves nothing behind that a later
// request could replay.
func TestARefusedRequestIsNeverRecorded(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	_, outsiderSession := f.auth.addUser(t, false)

	rec := f.doKeyedAs(outsiderSession.ID, http.MethodPost, f.base()+"/locations", `{"name":"Pantry"}`, uuid.NewString())

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Zero(t, f.idem.count())
	assert.Zero(t, f.locations.creates.Load())
}

// TestAReplayedDeleteIsTheOriginal204 — without a record, the retry of a
// delete that landed would be a 404 and the client would think it failed.
func TestAReplayedDeleteIsTheOriginal204(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := f.jobs.add(t, f.storageID, store.JobDone, `{}`)
	key := uuid.NewString()
	path := f.base() + "/jobs/" + job.ID.String()

	require.Equal(t, http.StatusNoContent, f.doKeyed(http.MethodDelete, path, "", key).Code)
	retry := f.doKeyed(http.MethodDelete, path, "", key)

	assert.Equal(t, http.StatusNoContent, retry.Code)
	assert.Empty(t, retry.Body.String())
	assert.Equal(t, "true", retry.Header().Get(httpapi.IdempotentReplayHeader))
}

func TestReadsIgnoreTheKey(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.doKeyed(http.MethodGet, f.base()+"/jobs", "", uuid.NewString())

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, f.idem.count(), "a read has nothing to deduplicate")
}

// TestIdempotencyCoversTheAuthAndAdminGroups — "any mutating request", not only
// the storage routes.
func TestIdempotencyCoversTheAuthAndAdminGroups(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	key := uuid.NewString()
	body := `{"name":"Garage"}`

	first := f.doKeyed(http.MethodPost, "/api/admin/storages", body, key)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	retry := f.doKeyed(http.MethodPost, "/api/admin/storages", body, key)
	assert.JSONEq(t, first.Body.String(), retry.Body.String(), "one storage, not two")

	storages, err := f.auth.ListStorages(context.Background())
	require.NoError(t, err)
	assert.Len(t, storages, 1)

	codeKey := uuid.NewString()
	codeFirst := f.doKeyed(http.MethodPost, "/api/auth/pairing-codes", "", codeKey)
	require.Equal(t, http.StatusCreated, codeFirst.Code)
	codeRetry := f.doKeyed(http.MethodPost, "/api/auth/pairing-codes", "", codeKey)
	assert.JSONEq(t, codeFirst.Body.String(), codeRetry.Body.String())
}
