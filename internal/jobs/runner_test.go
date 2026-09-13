package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// fakeStore mirrors the store's rules that matter to the runner: only a pending
// job can finish, and a deleted one is ErrNotFound.
type fakeStore struct {
	mu   sync.Mutex
	jobs map[uuid.UUID]*store.Job
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: map[uuid.UUID]*store.Job{}} }

func (f *fakeStore) CreateJob(_ context.Context, in store.NewJob) (*store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := &store.Job{ID: uuid.New(), StorageID: in.StorageID, Kind: in.Kind, Status: store.JobPending, CreatedBy: in.CreatedBy, ImageFilename: in.ImageFilename}
	f.jobs[j.ID] = j
	copied := *j
	return &copied, nil
}

func (f *fakeStore) CompleteJob(_ context.Context, id uuid.UUID, payload json.RawMessage) error {
	return f.finish(id, store.JobDone, payload, nil)
}

func (f *fakeStore) FailJob(_ context.Context, id uuid.UUID, message string) error {
	return f.finish(id, store.JobFailed, nil, &message)
}

func (f *fakeStore) finish(id uuid.UUID, status store.JobStatus, payload json.RawMessage, message *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok || j.Status != store.JobPending {
		return store.ErrNotFound
	}
	j.Status, j.Payload, j.Error = status, payload, message
	return nil
}

func (f *fakeStore) FailInterruptedJobs(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, j := range f.jobs {
		if j.Status == store.JobPending {
			msg := store.InterruptedJobError
			j.Status, j.Error = store.JobFailed, &msg
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) delete(id uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jobs, id)
}

func (f *fakeStore) get(id uuid.UUID) (store.Job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return store.Job{}, false
	}
	return *j, true
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// waitFor polls until the job leaves pending, so tests never sleep a guess.
func waitFor(t *testing.T, s *fakeStore, id uuid.UUID) store.Job {
	t.Helper()
	var got store.Job
	require.Eventually(t, func() bool {
		j, ok := s.get(id)
		got = j
		return ok && j.Status != store.JobPending
	}, 5*time.Second, 5*time.Millisecond)
	return got
}

func TestSubmitReturnsPendingAndRecordsTheProposal(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())
	release := make(chan struct{})

	job, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) {
			<-release
			return json.RawMessage(`{"items":[]}`), nil
		})
	require.NoError(t, err)
	assert.Equal(t, store.JobPending, job.Status, "Submit returns before the work finishes")

	close(release)
	got := waitFor(t, s, job.ID)
	assert.Equal(t, store.JobDone, got.Status)
	assert.JSONEq(t, `{"items":[]}`, string(got.Payload))
}

// TestSubmitDoesNotTieWorkToTheRequest — the request that submitted a job ends
// with its 202. If the work ran on that request's context it would be
// cancelled the moment the response was written.
func TestSubmitDoesNotTieWorkToTheRequest(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())
	reqCtx, endRequest := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})

	job, err := r.Submit(reqCtx, store.NewJob{StorageID: uuid.New(), Kind: store.JobProductPhoto},
		func(ctx context.Context) (json.RawMessage, error) {
			close(started)
			<-release
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return json.RawMessage(`{"ok":true}`), nil
		})
	require.NoError(t, err)

	<-started
	endRequest()
	close(release)

	assert.Equal(t, store.JobDone, waitFor(t, s, job.ID).Status)
}

// TestFailureMessagesAreWrittenForTheUser — a provider's own error text must
// not reach the review inbox; a UserError's message must.
func TestFailureMessagesAreWrittenForTheUser(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())

	internal, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) {
			return nil, errors.New("gemini: 403 API key AIza... invalid")
		})
	require.NoError(t, err)
	user, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) {
			return nil, &UserError{Message: "The photo is too dark to read.", Err: errors.New("low confidence")}
		})
	require.NoError(t, err)

	gotInternal := waitFor(t, s, internal.ID)
	require.NotNil(t, gotInternal.Error)
	assert.Equal(t, GenericFailure, *gotInternal.Error)

	gotUser := waitFor(t, s, user.ID)
	require.NotNil(t, gotUser.Error)
	assert.Equal(t, "The photo is too dark to read.", *gotUser.Error)
}

// TestAPanickingJobFailsAloneInsteadOfCrashing — one bad photo must not take
// the server, and every other job, down with it.
func TestAPanickingJobFailsAloneInsteadOfCrashing(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())

	bad, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) { panic("decoder exploded") })
	require.NoError(t, err)
	good, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	require.NoError(t, err)

	assert.Equal(t, store.JobFailed, waitFor(t, s, bad.ID).Status)
	assert.Equal(t, store.JobDone, waitFor(t, s, good.ID).Status)
}

func TestInvalidPayloadFailsTheJob(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())

	job, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{not json`), nil })
	require.NoError(t, err)

	got := waitFor(t, s, job.ID)
	assert.Equal(t, store.JobFailed, got.Status, "a payload the review page cannot parse is not a proposal")
}

func TestAJobThatOutlivesItsTimeoutFails(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())
	r.timeout = 20 * time.Millisecond

	job, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(ctx context.Context) (json.RawMessage, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	require.NoError(t, err)

	got := waitFor(t, s, job.ID)
	assert.Equal(t, store.JobFailed, got.Status)
	require.NotNil(t, got.Error)
	assert.Equal(t, TimeoutFailure, *got.Error)
}

// TestShutdownRecordsInterruptedWork — cancelled work is recorded as
// interrupted, with the same retryable message the restart sweep uses, and
// Submit is refused afterwards.
func TestShutdownRecordsInterruptedWork(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())
	started := make(chan struct{})

	job, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(ctx context.Context) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	require.NoError(t, err)
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, r.Shutdown(ctx))

	got, ok := s.get(job.ID)
	require.True(t, ok)
	assert.Equal(t, store.JobFailed, got.Status)
	require.NotNil(t, got.Error)
	assert.Equal(t, store.InterruptedJobError, *got.Error)

	_, err = r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	assert.ErrorIs(t, err, ErrShutDown)
}

// TestAJobDiscardedMidFlightStaysDiscarded — the result arriving after a
// DELETE must not recreate or resurrect anything, and must not be logged as a
// failure worth waking anyone for.
func TestAJobDiscardedMidFlightStaysDiscarded(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})

	job, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) {
			close(started)
			<-release
			defer close(finished)
			return json.RawMessage(`{}`), nil
		})
	require.NoError(t, err)

	<-started
	s.delete(job.ID)
	close(release)
	<-finished

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, r.Shutdown(ctx), "the discarded job's goroutine must still end")

	_, ok := s.get(job.ID)
	assert.False(t, ok)
}

// TestConcurrencyIsBounded — six photos uploaded at once are processed a few at
// a time, not as six simultaneous provider calls.
func TestConcurrencyIsBounded(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())

	var mu sync.Mutex
	running, peak := 0, 0
	release := make(chan struct{})

	var ids []uuid.UUID
	for range 6 {
		job, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
			func(context.Context) (json.RawMessage, error) {
				mu.Lock()
				running++
				if running > peak {
					peak = running
				}
				mu.Unlock()
				<-release
				mu.Lock()
				running--
				mu.Unlock()
				return json.RawMessage(`{}`), nil
			})
		require.NoError(t, err)
		ids = append(ids, job.ID)
	}

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return running == DefaultConcurrency
	}, 5*time.Second, 5*time.Millisecond)
	close(release)

	for _, id := range ids {
		waitFor(t, s, id)
	}
	assert.Equal(t, DefaultConcurrency, peak)
}

func TestRecoverFailsWhatThePreviousProcessLeftPending(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	orphan, err := s.CreateJob(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion})
	require.NoError(t, err)

	require.NoError(t, New(s, quietLogger()).Recover(context.Background()))

	got, _ := s.get(orphan.ID)
	assert.Equal(t, store.JobFailed, got.Status)
}
