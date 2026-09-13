package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// fakeJobs is an in-memory JobStore with the store's ordering (id descending,
// ids UUIDv7) and scoping (another storage's job is ErrNotFound).
type fakeJobs struct {
	mu        sync.Mutex
	jobs      map[uuid.UUID]*store.Job
	lastLimit int
}

func newFakeJobs() *fakeJobs { return &fakeJobs{jobs: map[uuid.UUID]*store.Job{}} }

func (f *fakeJobs) add(t *testing.T, storageID uuid.UUID, status store.JobStatus, payload string) *store.Job {
	t.Helper()
	id, err := uuid.NewV7()
	require.NoError(t, err)
	j := &store.Job{
		ID: id, StorageID: storageID, Kind: store.JobShelfIngestion, Status: status,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if payload != "" {
		j.Payload = json.RawMessage(payload)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id] = j
	return j
}

func (f *fakeJobs) Job(_ context.Context, storageID, id uuid.UUID) (*store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok || j.StorageID != storageID {
		return nil, store.ErrNotFound
	}
	copied := *j
	return &copied, nil
}

func (f *fakeJobs) ListJobs(_ context.Context, storageID uuid.UUID, statuses []store.JobStatus, after *uuid.UUID, limit int) ([]store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLimit = limit
	wanted := map[store.JobStatus]bool{}
	for _, s := range statuses {
		wanted[s] = true
	}
	var out []store.Job
	for _, j := range f.jobs {
		if j.StorageID != storageID || !wanted[j.Status] {
			continue
		}
		if after != nil && bytes.Compare(j.ID[:], after[:]) >= 0 {
			continue
		}
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool { return bytes.Compare(out[a].ID[:], out[b].ID[:]) > 0 })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeJobs) DeleteJob(_ context.Context, storageID, id uuid.UUID) (*string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok || j.StorageID != storageID {
		return nil, store.ErrNotFound
	}
	delete(f.jobs, id)
	return j.ImageFilename, nil
}

type jobsPage struct {
	Items []struct {
		ID      uuid.UUID       `json:"id"`
		Status  string          `json:"status"`
		Payload json.RawMessage `json:"payload"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func decodeJobsPage(t *testing.T, body []byte) jobsPage {
	t.Helper()
	var page jobsPage
	require.NoError(t, json.Unmarshal(body, &page))
	return page
}

// TestJobGetCarriesThePayloadThePollerReads is the contract js/jobs.js polls
// against: status, and the proposal once done.
func TestJobGetCarriesThePayloadThePollerReads(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := f.jobs.add(t, f.storageID, store.JobDone, `{"items":[{"name":"Beans"}]}`)

	rec := f.do(http.MethodGet, f.base()+"/jobs/"+job.ID.String(), "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		ID      uuid.UUID       `json:"id"`
		Status  string          `json:"status"`
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
		Error   *string         `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, job.ID, body.ID)
	assert.Equal(t, "done", body.Status)
	assert.Equal(t, "shelf_ingestion", body.Kind)
	assert.JSONEq(t, `{"items":[{"name":"Beans"}]}`, string(body.Payload))
	assert.Nil(t, body.Error)
}

// TestAnotherStoragesJobIsAPlain404 — one storage's members cannot poll, list
// or discard another's jobs, and cannot tell those jobs exist.
func TestAnotherStoragesJobIsAPlain404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	foreign := f.jobs.add(t, uuid.New(), store.JobDone, `{"secret":true}`)

	missing := f.do(http.MethodGet, f.base()+"/jobs/"+uuid.NewString(), "")
	notOurs := f.do(http.MethodGet, f.base()+"/jobs/"+foreign.ID.String(), "")
	malformed := f.do(http.MethodGet, f.base()+"/jobs/not-a-uuid", "")

	require.Equal(t, http.StatusNotFound, notOurs.Code)
	assert.Equal(t, missing.Body.String(), notOurs.Body.String())
	assert.Equal(t, missing.Body.String(), malformed.Body.String())

	assert.Equal(t, http.StatusNotFound, f.do(http.MethodDelete, f.base()+"/jobs/"+foreign.ID.String(), "").Code)
	_, err := f.jobs.Job(context.Background(), foreign.StorageID, foreign.ID)
	assert.NoError(t, err, "a refused discard must not discard")

	list := f.do(http.MethodGet, f.base()+"/jobs", "")
	assert.NotContains(t, list.Body.String(), foreign.ID.String())
}

// TestJobInboxExcludesAppliedJobsByDefault — with no status filter the list is
// the review inbox: everything not yet consumed.
func TestJobInboxExcludesAppliedJobsByDefault(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	pending := f.jobs.add(t, f.storageID, store.JobPending, "")
	done := f.jobs.add(t, f.storageID, store.JobDone, `{}`)
	failed := f.jobs.add(t, f.storageID, store.JobFailed, "")
	consumed := f.jobs.add(t, f.storageID, store.JobConsumed, `{}`)

	rec := f.do(http.MethodGet, f.base()+"/jobs", "")
	require.Equal(t, http.StatusOK, rec.Code)
	page := decodeJobsPage(t, rec.Body.Bytes())

	var ids []uuid.UUID
	for _, item := range page.Items {
		ids = append(ids, item.ID)
		assert.Empty(t, item.Payload, "the list carries no proposals")
	}
	assert.Equal(t, []uuid.UUID{failed.ID, done.ID, pending.ID}, ids, "newest first, consumed excluded")
	assert.NotContains(t, ids, consumed.ID)
	assert.Nil(t, page.NextCursor)

	onlyDone := decodeJobsPage(t, f.do(http.MethodGet, f.base()+"/jobs?status=done", "").Body.Bytes())
	require.Len(t, onlyDone.Items, 1)
	assert.Equal(t, done.ID, onlyDone.Items[0].ID)

	two := decodeJobsPage(t, f.do(http.MethodGet, f.base()+"/jobs?status=done,consumed", "").Body.Bytes())
	assert.Len(t, two.Items, 2)

	bad := f.do(http.MethodGet, f.base()+"/jobs?status=finished", "")
	assert.Equal(t, http.StatusUnprocessableEntity, bad.Code)
	assert.Contains(t, bad.Body.String(), `"status"`)
}

// TestJobListPagesByCursor walks a collection to the end, the way a client
// has to: echo next_cursor until it is null.
func TestJobListPagesByCursor(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	var created []uuid.UUID
	for range 5 {
		created = append(created, f.jobs.add(t, f.storageID, store.JobDone, `{}`).ID)
	}

	var seen []uuid.UUID
	path := f.base() + "/jobs?limit=2"
	for pages := 0; ; pages++ {
		require.Less(t, pages, 5, "pagination must terminate")
		rec := f.do(http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		page := decodeJobsPage(t, rec.Body.Bytes())
		assert.LessOrEqual(t, len(page.Items), 2)
		for _, item := range page.Items {
			seen = append(seen, item.ID)
		}
		if page.NextCursor == nil {
			break
		}
		_, parseErr := uuid.Parse(*page.NextCursor)
		assert.Error(t, parseErr, "the cursor is opaque, not a bare id")
		path = f.base() + "/jobs?limit=2&cursor=" + *page.NextCursor
	}

	require.Len(t, seen, 5, "every job exactly once")
	for i := range created {
		assert.Equal(t, created[len(created)-1-i], seen[i])
	}

	// An exact multiple of the limit must not end on an empty page with a
	// cursor pointing nowhere.
	exact := decodeJobsPage(t, f.do(http.MethodGet, f.base()+"/jobs?limit=5", "").Body.Bytes())
	assert.Len(t, exact.Items, 5)
	assert.Nil(t, exact.NextCursor)
}

func TestPaginationParametersAreValidated(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	for _, bad := range []string{"limit=0", "limit=-3", "limit=ten", "cursor=not!base64", "cursor=AAAA", "cursor=" + uuid.NewString()} {
		rec := f.do(http.MethodGet, f.base()+"/jobs?"+bad, "")
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, bad)
	}

	require.Equal(t, http.StatusOK, f.do(http.MethodGet, f.base()+"/jobs", "").Code)
	assert.Equal(t, 51, f.jobs.lastLimit, "default 50, fetched one over to detect the last page")

	require.Equal(t, http.StatusOK, f.do(http.MethodGet, f.base()+"/jobs?limit=5000", "").Code)
	assert.Equal(t, 201, f.jobs.lastLimit, "capped at 200")

	require.Equal(t, http.StatusOK, f.do(http.MethodGet, f.base()+"/jobs?limit="+strconv.Itoa(7), "").Code)
	assert.Equal(t, 8, f.jobs.lastLimit)
}

func TestDiscardingAJob(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := f.jobs.add(t, f.storageID, store.JobDone, `{}`)

	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.base()+"/jobs/"+job.ID.String(), "").Code)
	assert.Equal(t, http.StatusNotFound, f.do(http.MethodGet, f.base()+"/jobs/"+job.ID.String(), "").Code)
	assert.Equal(t, http.StatusNotFound, f.do(http.MethodDelete, f.base()+"/jobs/"+job.ID.String(), "").Code)
}
