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

// claim is what jobs.lease_owner and jobs.lease_expires_at hold for one row.
//
// held distinguishes "no owner" from "owned by the zero UUID", because the
// store's sweep does: its predicate is `lease_owner IS DISTINCT FROM $1`, under
// which a NULL owner never matches any caller. A fake that compared zero values
// would quietly skip unclaimed rows.
type claim struct {
	held    bool
	owner   uuid.UUID
	expires time.Time
}

// claimFrom mirrors the store writing a lease: an unowned lease writes NULL for
// both columns, and an owned one an expiry TTL from now.
func claimFrom(l store.JobLease) claim {
	if l.Owner == uuid.Nil {
		return claim{}
	}
	return claim{held: true, owner: l.Owner, expires: time.Now().Add(l.TTL)}
}

// lapsed reports what the sweep's `lease_expires_at IS NULL OR < now()` reports.
func (c claim) lapsed() bool { return !c.held || !c.expires.After(time.Now()) }

// fakeStore mirrors the store's rules that matter to the runner: only a pending
// job can finish, a deleted one is ErrNotFound, and a pending row carries the
// claim of the process working it.
type fakeStore struct {
	mu     sync.Mutex
	jobs   map[uuid.UUID]*store.Job
	claims map[uuid.UUID]claim
}

func newFakeStore() *fakeStore {
	return &fakeStore{jobs: map[uuid.UUID]*store.Job{}, claims: map[uuid.UUID]claim{}}
}

func (f *fakeStore) CreateJob(_ context.Context, in store.NewJob) (*store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := &store.Job{ID: uuid.New(), StorageID: in.StorageID, Kind: in.Kind, Status: store.JobPending, CreatedBy: in.CreatedBy, ImageFilename: in.ImageFilename}
	f.jobs[j.ID] = j
	f.claims[j.ID] = claimFrom(in.Lease)
	copied := *j
	return &copied, nil
}

// RequeueJob mirrors the store: only a done or failed job with a photo, in its
// own storage, moves back to pending — claimed by whoever requeued it.
func (f *fakeStore) RequeueJob(_ context.Context, storageID, id uuid.UUID, lease store.JobLease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok || j.StorageID != storageID {
		return store.ErrNotFound
	}
	if (j.Status != store.JobDone && j.Status != store.JobFailed) || j.ImageFilename == nil {
		return store.ErrConflict
	}
	j.Status, j.Payload, j.Error = store.JobPending, nil, nil
	f.claims[id] = claimFrom(lease)
	return nil
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
	f.claims[id] = claim{}
	return nil
}

// FailOrphanedJobs mirrors the store's predicate: pending, not claimed by the
// caller itself, and either never claimed or claimed by a process that stopped
// renewing.
func (f *fakeStore) FailOrphanedJobs(_ context.Context, owner uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, j := range f.jobs {
		c := f.claims[id]
		if j.Status != store.JobPending || (c.held && c.owner == owner) || !c.lapsed() {
			continue
		}
		msg := store.InterruptedJobError
		j.Status, j.Error = store.JobFailed, &msg
		f.claims[id] = claim{}
		n++
	}
	return n, nil
}

func (f *fakeStore) RenewJobLeases(_ context.Context, lease store.JobLease) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, j := range f.jobs {
		if c := f.claims[id]; j.Status == store.JobPending && c.held && c.owner == lease.Owner {
			f.claims[id] = claimFrom(lease)
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) ReleaseJobLeases(_ context.Context, owner uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, j := range f.jobs {
		if c := f.claims[id]; j.Status == store.JobPending && c.held && c.owner == owner {
			f.claims[id] = claim{}
			n++
		}
	}
	return n, nil
}

// claimOf reports what a row's lease columns hold, for the tests below.
func (f *fakeStore) claimOf(id uuid.UUID) claim {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims[id]
}

// claimFor plants a pending job owned by another process, as the second instance
// of a rolling update leaves behind. A TTL in the past is one whose owner stopped
// renewing it.
func (f *fakeStore) claimFor(t *testing.T, owner uuid.UUID, ttl time.Duration) uuid.UUID {
	t.Helper()
	job, err := f.CreateJob(context.Background(), store.NewJob{
		StorageID: uuid.New(),
		Kind:      store.JobShelfIngestion,
		Lease:     store.JobLease{Owner: owner, TTL: ttl},
	})
	require.NoError(t, err)
	return job.ID
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

// TestRecoverLeavesAnotherLiveInstancesWorkAlone is issue #121: the rolling
// update on the NAS starts a second instance next to the first on purpose
// (deploy/synology/update), and the second one's start-up used to fail every
// pending job — including the ones the first instance was in the middle of.
//
// The three rows here are the whole rule. A claim another process is still
// renewing is live work and survives; a claim nobody renewed is a process that
// died and is failed; a row with no claim at all — an old binary's insert, or a
// claim given up at shutdown — is failed too, which is what keeps stop-then-start
// behaving exactly as it always has.
func TestRecoverLeavesAnotherLiveInstancesWorkAlone(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	otherInstance := uuid.New()
	live := s.claimFor(t, otherInstance, time.Hour)
	abandoned := s.claimFor(t, otherInstance, -time.Second)
	unclaimed, err := s.CreateJob(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion})
	require.NoError(t, err)

	require.NoError(t, New(s, quietLogger()).Recover(context.Background()))

	got, _ := s.get(live)
	assert.Equal(t, store.JobPending, got.Status, "a job the other instance is still working")
	assert.Equal(t, otherInstance, s.claimOf(live).owner, "and its claim is left as it was")

	got, _ = s.get(abandoned)
	assert.Equal(t, store.JobFailed, got.Status, "a claim nobody renews is a process that is gone")
	require.NotNil(t, got.Error)
	assert.Equal(t, store.InterruptedJobError, *got.Error)

	got, _ = s.get(unclaimed.ID)
	assert.Equal(t, store.JobFailed, got.Status, "a pending row nobody claims is nobody's work")
}

// TestSubmitClaimsTheJobForThisProcess — the claim is written by the insert, so
// there is no instant in which a pending row is unowned and another instance's
// recovery could fail it.
func TestSubmitClaimsTheJobForThisProcess(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r, other := New(s, quietLogger()), New(s, quietLogger())
	assert.NotEqual(t, r.owner, other.owner, "two processes never share an owner")

	release := make(chan struct{})
	job, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) {
			<-release
			return json.RawMessage(`{}`), nil
		})
	require.NoError(t, err)

	held := s.claimOf(job.ID)
	assert.True(t, held.held)
	assert.Equal(t, r.owner, held.owner)
	assert.False(t, held.lapsed(), "the claim is current from the moment the row exists")

	// The other instance starting up now must not touch it.
	require.NoError(t, other.Recover(context.Background()))
	got, _ := s.get(job.ID)
	assert.Equal(t, store.JobPending, got.Status)

	close(release)
	assert.Equal(t, store.JobDone, waitFor(t, s, job.ID).Status)
	assert.False(t, s.claimOf(job.ID).held, "a job that is no longer pending holds no claim")
}

// TestTheLeaseKeeperRenewsItsOwnAndFailsTheAbandoned — the two halves of one
// tick. Renewing is what tells the other instance this one's work is alive;
// sweeping is what recovers an instance that was killed while this one kept
// serving, which no start-up will ever look at again (issue #124, item 4).
func TestTheLeaseKeeperRenewsItsOwnAndFailsTheAbandoned(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())
	r.leaseTTL = 500 * time.Millisecond
	r.leaseInterval = 5 * time.Millisecond

	release := make(chan struct{})
	mine, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) {
			<-release
			return json.RawMessage(`{}`), nil
		})
	require.NoError(t, err)
	abandoned := s.claimFor(t, uuid.New(), -time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunLeases(ctx)

	require.Eventually(t, func() bool {
		got, ok := s.get(abandoned)
		return ok && got.Status == store.JobFailed
	}, 5*time.Second, 5*time.Millisecond, "a lapsed claim of another process is swept while this one runs")

	// Its own job is still pending well past the TTL, because the keeper keeps
	// renewing the claim.
	firstSeen := s.claimOf(mine.ID).expires
	require.Eventually(t, func() bool { return s.claimOf(mine.ID).expires.After(firstSeen) },
		5*time.Second, 5*time.Millisecond, "the claim is pushed forward")
	got, _ := s.get(mine.ID)
	assert.Equal(t, store.JobPending, got.Status, "the keeper never fails its own work")

	close(release)
	assert.Equal(t, store.JobDone, waitFor(t, s, mine.ID).Status)
}

// TestShutdownGivesUpTheClaimsItCouldNotFinish — a job whose outcome could not
// be written inside the shutdown grace stays pending, and giving up its claim is
// what lets the next process fail it at once instead of waiting out a lease
// nobody will renew. That is the stop-then-start behaviour this feature must not
// change.
func TestShutdownGivesUpTheClaimsItCouldNotFinish(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())
	started := make(chan struct{})
	release := make(chan struct{})

	job, err := r.Submit(context.Background(), store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) {
			close(started)
			<-release // deaf to cancellation, like a provider call mid-flight
			return json.RawMessage(`{}`), nil
		})
	require.NoError(t, err)
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, r.Shutdown(ctx), context.DeadlineExceeded)

	got, _ := s.get(job.ID)
	require.Equal(t, store.JobPending, got.Status, "the work never got to record an outcome")
	assert.False(t, s.claimOf(job.ID).held, "the claim is given up on the way out")

	require.NoError(t, New(s, quietLogger()).Recover(context.Background()))
	got, _ = s.get(job.ID)
	assert.Equal(t, store.JobFailed, got.Status, "the next start-up fails it immediately, as it always did")

	close(release)
}

// TestResubmitAnalysesAFinishedJobAgain — "Analyze again" puts a finished job
// back to pending, runs the new work, and records its result on the same row.
// A job the store refuses to requeue starts no work at all.
func TestResubmitAnalysesAFinishedJobAgain(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	r := New(s, quietLogger())
	storageID := uuid.New()
	photo := uuid.NewString() + ".jpg"

	job, err := r.Submit(context.Background(), store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion, ImageFilename: &photo},
		func(context.Context) (json.RawMessage, error) {
			return nil, &UserError{Message: "The photo could not be analysed."}
		})
	require.NoError(t, err)
	require.Equal(t, store.JobFailed, waitFor(t, s, job.ID).Status)

	release := make(chan struct{})
	require.NoError(t, r.Resubmit(context.Background(), storageID, job.ID,
		func(context.Context) (json.RawMessage, error) {
			<-release
			return json.RawMessage(`{"rows":[{"row_id":"0"}]}`), nil
		}))

	pending, ok := s.get(job.ID)
	require.True(t, ok)
	assert.Equal(t, store.JobPending, pending.Status, "Resubmit returns before the work finishes")
	assert.Nil(t, pending.Error, "the old failure is gone while the new analysis runs")

	// The claim matters as much as the status. A re-analysis makes the row
	// pending again, and one this process left unclaimed would be failed by this
	// process's *own* next lease sweep while the work it just started is still
	// running — after which the result lands on a row that is no longer pending
	// and is dropped. That is issue #121's effect on the reanalyze path, so the
	// sweep is run here rather than only the claim inspected.
	held := s.claimOf(job.ID)
	assert.True(t, held.held, "Resubmit claims the job it made pending")
	assert.Equal(t, r.owner, held.owner)
	assert.False(t, held.lapsed())

	swept, err := s.FailOrphanedJobs(context.Background(), r.owner)
	require.NoError(t, err)
	assert.Zero(t, swept, "a re-analysis this process is running is not an orphan")
	running, ok := s.get(job.ID)
	require.True(t, ok)
	assert.Equal(t, store.JobPending, running.Status)

	// A second request while the first re-analysis is running is refused, and
	// its work never runs.
	ran := make(chan struct{}, 1)
	refused := func(context.Context) (json.RawMessage, error) {
		ran <- struct{}{}
		return json.RawMessage(`{}`), nil
	}
	assert.ErrorIs(t, r.Resubmit(context.Background(), storageID, job.ID, refused), store.ErrConflict)
	assert.ErrorIs(t, r.Resubmit(context.Background(), uuid.New(), job.ID, refused), store.ErrNotFound, "another storage's job")

	close(release)
	got := waitFor(t, s, job.ID)
	assert.Equal(t, store.JobDone, got.Status)
	assert.JSONEq(t, `{"rows":[{"row_id":"0"}]}`, string(got.Payload))

	noPhoto, err := r.Submit(context.Background(), store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion},
		func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	require.NoError(t, err)
	waitFor(t, s, noPhoto.ID)
	assert.ErrorIs(t, r.Resubmit(context.Background(), storageID, noPhoto.ID, refused), store.ErrConflict, "nothing to analyse")

	require.NoError(t, r.Shutdown(context.Background()))
	assert.ErrorIs(t, r.Resubmit(context.Background(), storageID, job.ID, refused), ErrShutDown)

	select {
	case <-ran:
		t.Fatal("work for a refused Resubmit ran")
	default:
	}
}
