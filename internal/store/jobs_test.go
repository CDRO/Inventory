package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

func jobStatus(t *testing.T, ctx context.Context, id uuid.UUID) string {
	t.Helper()
	var status string
	require.NoError(t, testPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, id).Scan(&status))
	return status
}

// jobClaim reads a job's lease columns: who owns the pending row, and when the
// claim lapses. Both nil is an unclaimed row (migrations/00014_job_lease.sql).
func jobClaim(t *testing.T, ctx context.Context, id uuid.UUID) (*uuid.UUID, *time.Time) {
	t.Helper()
	var owner *uuid.UUID
	var expires *time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT lease_owner, lease_expires_at FROM jobs WHERE id = $1`, id).Scan(&owner, &expires))
	return owner, expires
}

// plantPendingJob inserts a pending job with its lease columns written by hand,
// which is how a row another process left behind is reproduced: a fixture built
// through CreateJob would prove nothing about rows CreateJob did not write, and
// those — an old binary's insert, a claim given up at shutdown, a claim nobody
// renewed — are exactly what recovery has to judge.
//
// owner uuid.Nil writes NULL, and so does a zero expiresIn: together they are the
// unclaimed state. A negative expiresIn is a claim whose owner stopped renewing
// it.
func plantPendingJob(t *testing.T, ctx context.Context, storageID, owner uuid.UUID, expiresIn time.Duration) uuid.UUID {
	t.Helper()

	id, err := uuid.NewV7()
	require.NoError(t, err)

	var ownerArg *uuid.UUID
	if owner != uuid.Nil {
		ownerArg = &owner
	}
	var secs *float64
	if expiresIn != 0 {
		s := expiresIn.Seconds()
		secs = &s
	}
	_, err = execTest(ctx, `
		INSERT INTO jobs (id, storage_id, kind, status, lease_owner, lease_expires_at)
		VALUES ($1, $2, 'shelf_ingestion', 'pending', $3, now() + make_interval(secs => $4))`,
		id, storageID, ownerArg, secs)
	require.NoError(t, err)
	return id
}

func TestJobLifecyclePendingToDone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion, CreatedBy: &userID})
	require.NoError(t, err)
	assert.Equal(t, store.JobPending, job.Status)
	assert.Equal(t, "pending", jobStatus(t, ctx, job.ID))

	require.NoError(t, s.CompleteJob(ctx, job.ID, json.RawMessage(`{"items":[1,2]}`)))

	got, err := s.Job(ctx, storageID, job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.JobDone, got.Status)
	assert.JSONEq(t, `{"items":[1,2]}`, string(got.Payload))
	assert.Nil(t, got.Error)
}

// TestOnlyAPendingJobCanFinish — a job discarded mid-flight, or already failed
// by the restart sweep, must not be resurrected by a vision call that returns
// late.
func TestOnlyAPendingJobCanFinish(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobProductPhoto})
	require.NoError(t, err)
	require.NoError(t, s.FailJob(ctx, job.ID, "nope"))

	assert.ErrorIs(t, s.CompleteJob(ctx, job.ID, json.RawMessage(`{}`)), store.ErrNotFound)
	assert.Equal(t, "failed", jobStatus(t, ctx, job.ID), "a late result must not overwrite the failure")

	discarded, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobProductPhoto})
	require.NoError(t, err)
	_, err = s.DeleteJob(ctx, storageID, discarded.ID)
	require.NoError(t, err)
	assert.ErrorIs(t, s.CompleteJob(ctx, discarded.ID, json.RawMessage(`{}`)), store.ErrNotFound)
}

// TestFailOrphanedJobsLeavesFinishedOnesAlone — the restart sweep fails what
// was pending and unowned, and touches nothing else.
func TestFailOrphanedJobsLeavesFinishedOnesAlone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	pending, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	done, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	require.NoError(t, s.CompleteJob(ctx, done.ID, json.RawMessage(`{"ok":true}`)))

	n, err := s.FailOrphanedJobs(ctx, uuid.Must(uuid.NewV7()))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))

	got, err := s.Job(ctx, storageID, pending.ID)
	require.NoError(t, err)
	assert.Equal(t, store.JobFailed, got.Status)
	require.NotNil(t, got.Error)
	assert.Equal(t, store.InterruptedJobError, *got.Error)

	assert.Equal(t, "done", jobStatus(t, ctx, done.ID), "a finished proposal survives a restart")
}

// TestFailOrphanedJobsSparesWorkALiveProcessOwns is issue #121 at the statement
// that used to cause it. Recovery ran `WHERE status = 'pending'` and nothing
// else, on the reasoning that nothing could be working a pending row while it
// ran — true of one process, false during the rolling update on the NAS, which
// starts a second instance next to the first on purpose
// (deploy/synology/update).
//
// Four rows, four rules:
//   - a claim another process is still renewing is live work: left alone;
//   - a claim nobody renewed is a process that died: failed;
//   - no claim at all — an old binary's insert, or a claim given up at
//     shutdown — is nobody's work: failed, which is what keeps stop-then-start
//     behaving as it always has;
//   - a lapsed claim of the sweeping process itself is left alone, because a
//     renewal of ours that did not get through is evidence about the database,
//     not about our own goroutines. Another instance still fails it.
func TestFailOrphanedJobsSparesWorkALiveProcessOwns(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	me := uuid.Must(uuid.NewV7())
	otherInstance := uuid.Must(uuid.NewV7())

	live := plantPendingJob(t, ctx, storageID, otherInstance, time.Hour)
	abandoned := plantPendingJob(t, ctx, storageID, otherInstance, -time.Second)
	unclaimed := plantPendingJob(t, ctx, storageID, uuid.Nil, 0)
	mineStalled := plantPendingJob(t, ctx, storageID, me, -time.Second)

	n, err := s.FailOrphanedJobs(ctx, me)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(2))

	assert.Equal(t, "pending", jobStatus(t, ctx, live), "the other instance is still working this one")
	owner, expires := jobClaim(t, ctx, live)
	require.NotNil(t, owner)
	assert.Equal(t, otherInstance, *owner, "and its claim is untouched")
	require.NotNil(t, expires)

	assert.Equal(t, "failed", jobStatus(t, ctx, abandoned), "a claim nobody renews is a process that is gone")
	owner, expires = jobClaim(t, ctx, abandoned)
	assert.Nil(t, owner, "a job that is no longer pending holds no claim")
	assert.Nil(t, expires)

	assert.Equal(t, "failed", jobStatus(t, ctx, unclaimed), "a pending row nobody claims is nobody's work")

	assert.Equal(t, "pending", jobStatus(t, ctx, mineStalled),
		"our own lapsed claim is a failed renewal, not a dead goroutine")

	// The same row, swept by any other process, is failed: that is what makes an
	// instance killed while another kept serving recoverable at all
	// (issue #124, item 4).
	_, err = s.FailOrphanedJobs(ctx, otherInstance)
	require.NoError(t, err)
	assert.Equal(t, "failed", jobStatus(t, ctx, mineStalled))
}

// TestRenewJobLeasesOnlyPushesItsOwnClaims — the heartbeat, and the one thing it
// must not do: touch updated_at, which is the job's own last change and what the
// retention sweep and every client order by. A claim renewed every few seconds
// is bookkeeping about a process, not a change to the job.
func TestRenewJobLeasesOnlyPushesItsOwnClaims(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	me := uuid.Must(uuid.NewV7())
	otherInstance := uuid.Must(uuid.NewV7())
	mine := plantPendingJob(t, ctx, storageID, me, time.Second)
	theirs := plantPendingJob(t, ctx, storageID, otherInstance, time.Second)

	var before time.Time
	require.NoError(t, testPool.QueryRow(ctx, `SELECT updated_at FROM jobs WHERE id = $1`, mine).Scan(&before))
	_, mineExpiredAt := jobClaim(t, ctx, mine)
	require.NotNil(t, mineExpiredAt)
	_, theirsExpiredAt := jobClaim(t, ctx, theirs)
	require.NotNil(t, theirsExpiredAt)

	n, err := s.RenewJobLeases(ctx, store.JobLease{Owner: me, TTL: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only this process's claims")

	_, renewed := jobClaim(t, ctx, mine)
	require.NotNil(t, renewed)
	assert.True(t, renewed.After(*mineExpiredAt), "the claim is pushed forward")

	_, untouched := jobClaim(t, ctx, theirs)
	require.NotNil(t, untouched)
	assert.True(t, untouched.Equal(*theirsExpiredAt), "another process's claim is not ours to renew")

	var after time.Time
	require.NoError(t, testPool.QueryRow(ctx, `SELECT updated_at FROM jobs WHERE id = $1`, mine).Scan(&after))
	assert.True(t, after.Equal(before), "renewing a claim is not a change to the job")

	_, err = s.RenewJobLeases(ctx, store.JobLease{TTL: time.Hour})
	assert.ErrorIs(t, err, store.ErrValidation, "a lease with no owner claims nothing")
}

// TestReleaseJobLeasesLeavesTheRowPendingAndUnowned — what a process does on its
// way out for the jobs whose outcome it could not record in time. The row stays
// pending, so a straggler goroutine can still finish it, and it stops naming an
// owner, so the next start-up fails it at once instead of waiting out a claim
// nobody will renew.
func TestReleaseJobLeasesLeavesTheRowPendingAndUnowned(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	me := uuid.Must(uuid.NewV7())
	otherInstance := uuid.Must(uuid.NewV7())
	mine := plantPendingJob(t, ctx, storageID, me, time.Hour)
	theirs := plantPendingJob(t, ctx, storageID, otherInstance, time.Hour)

	n, err := s.ReleaseJobLeases(ctx, me)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	assert.Equal(t, "pending", jobStatus(t, ctx, mine), "releasing a claim does not fail the job")
	owner, expires := jobClaim(t, ctx, mine)
	assert.Nil(t, owner)
	assert.Nil(t, expires)

	owner, _ = jobClaim(t, ctx, theirs)
	require.NotNil(t, owner)
	assert.Equal(t, otherInstance, *owner, "another process's claim is not ours to give up")

	// And an unowned pending row is what the next start-up fails.
	_, err = s.FailOrphanedJobs(ctx, uuid.Must(uuid.NewV7()))
	require.NoError(t, err)
	assert.Equal(t, "failed", jobStatus(t, ctx, mine))
}

// TestCreateJobClaimsTheJobInTheSameStatement — there must be no instant in
// which a row is pending and unclaimed, because another instance's recovery
// running in that instant would fail it. The claim is written by the insert, and
// dropped again by the row's outcome.
func TestCreateJobClaimsTheJobInTheSameStatement(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	me := uuid.Must(uuid.NewV7())
	job, err := s.CreateJob(ctx, store.NewJob{
		StorageID: storageID,
		Kind:      store.JobShelfIngestion,
		Lease:     store.JobLease{Owner: me, TTL: time.Hour},
	})
	require.NoError(t, err)

	owner, expires := jobClaim(t, ctx, job.ID)
	require.NotNil(t, owner)
	assert.Equal(t, me, *owner)
	require.NotNil(t, expires)
	assert.True(t, expires.After(time.Now()), "the claim is current from the moment the row exists")

	require.NoError(t, s.CompleteJob(ctx, job.ID, json.RawMessage(`{"ok":true}`)))
	owner, expires = jobClaim(t, ctx, job.ID)
	assert.Nil(t, owner, "a job that is no longer pending holds no claim")
	assert.Nil(t, expires)

	// A job created with no lease at all is unowned, and that is a state the
	// sweep is meant to fail rather than an error to reject: nothing is working
	// a row nobody claimed.
	unowned, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	owner, expires = jobClaim(t, ctx, unowned.ID)
	assert.Nil(t, owner)
	assert.Nil(t, expires)
}

func TestJobsAreStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	mine := newStorage(t, ctx)
	theirs := newStorage(t, ctx)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: theirs, Kind: store.JobShelfIngestion})
	require.NoError(t, err)

	_, err = s.Job(ctx, mine, job.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.DeleteJob(ctx, mine, job.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, "pending", jobStatus(t, ctx, job.ID), "a refused delete must not delete")

	listed, err := s.ListJobs(ctx, mine, []store.JobStatus{store.JobPending}, nil, 50)
	require.NoError(t, err)
	assert.Empty(t, listed)

	_, err = s.CreateJob(ctx, store.NewJob{StorageID: uuid.New(), Kind: store.JobShelfIngestion})
	assert.ErrorIs(t, err, store.ErrNotFound, "a job for a storage that does not exist")
}

// TestListJobsPagesNewestFirstWithoutDrift — a job created between two page
// requests must not shift the second page, which is the offset failure mode.
func TestListJobsPagesNewestFirstWithoutDrift(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	var ids []uuid.UUID
	for range 5 {
		job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
		require.NoError(t, err)
		ids = append(ids, job.ID)
	}
	consumed, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	_, err = execTest(ctx, `UPDATE jobs SET status = 'consumed' WHERE id = $1`, consumed.ID)
	require.NoError(t, err)

	inbox := []store.JobStatus{store.JobPending, store.JobDone, store.JobFailed}

	first, err := s.ListJobs(ctx, storageID, inbox, nil, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, ids[4], first[0].ID, "newest first")
	assert.Equal(t, ids[3], first[1].ID)

	// Arrives between page requests.
	_, err = s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)

	second, err := s.ListJobs(ctx, storageID, inbox, &first[1].ID, 2)
	require.NoError(t, err)
	require.Len(t, second, 2)
	assert.Equal(t, ids[2], second[0].ID, "the page boundary does not move")
	assert.Equal(t, ids[1], second[1].ID)

	last, err := s.ListJobs(ctx, storageID, inbox, &second[1].ID, 2)
	require.NoError(t, err)
	require.Len(t, last, 1)
	assert.Equal(t, ids[0], last[0].ID)

	for _, page := range [][]store.Job{first, second, last} {
		for _, j := range page {
			assert.NotEqual(t, consumed.ID, j.ID, "a status filter excludes consumed jobs")
		}
	}
}

// TestDiscardJobsDeletesTheStatusAndTimeWindow — "Discard all"
// (docs/specs/32-inbox-discard-all.md): pending, done and failed jobs at or
// before the boundary go, a job just after it survives, a consumed job is
// never touched regardless of its age, and another storage's jobs are
// untouched even when they are otherwise in scope.
func TestDiscardJobsDeletesTheStatusAndTimeWindow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	other := newStorage(t, ctx)

	boundary := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	before := boundary.Add(-time.Hour)
	after := boundary.Add(time.Hour)

	newJobAt := func(storage uuid.UUID, status store.JobStatus, at time.Time) uuid.UUID {
		job, err := s.CreateJob(ctx, store.NewJob{StorageID: storage, Kind: store.JobShelfIngestion})
		require.NoError(t, err)
		_, err = execTest(ctx, `UPDATE jobs SET status = $2, created_at = $3 WHERE id = $1`, job.ID, status, at)
		require.NoError(t, err)
		return job.ID
	}

	pending := newJobAt(storageID, store.JobPending, before)
	done := newJobAt(storageID, store.JobDone, boundary) // exactly at the boundary: included
	failed := newJobAt(storageID, store.JobFailed, before)
	tooNew := newJobAt(storageID, store.JobDone, after)
	consumed := newJobAt(storageID, store.JobConsumed, before)
	foreign := newJobAt(other, store.JobPending, before)

	discarded, err := s.DiscardJobs(ctx, storageID, boundary)
	require.NoError(t, err)

	var got []uuid.UUID
	for _, d := range discarded {
		got = append(got, d.ID)
	}
	assert.ElementsMatch(t, []uuid.UUID{pending, done, failed}, got)

	assert.Equal(t, "done", jobStatus(t, ctx, tooNew), "created after the boundary must survive")
	assert.Equal(t, "consumed", jobStatus(t, ctx, consumed), "a consumed job is never touched")
	assert.Equal(t, "pending", jobStatus(t, ctx, foreign), "another storage's job must survive")

	for _, id := range []uuid.UUID{pending, done, failed} {
		_, err := s.Job(ctx, storageID, id)
		assert.ErrorIs(t, err, store.ErrNotFound)
	}

	// Nothing left to discard a second time.
	again, err := s.DiscardJobs(ctx, storageID, boundary)
	require.NoError(t, err)
	assert.Empty(t, again)
}

// TestConsumeJobAppliesAProposalOnce — the second confirm of the same job is a
// conflict, and a job that is not done cannot be consumed at all.
func TestConsumeJobAppliesAProposalOnce(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)

	consume := func(storage uuid.UUID) error {
		tx, err := testPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		if err := store.ConsumeJob(ctx, tx, storage, job.ID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	assert.ErrorIs(t, consume(storageID), store.ErrConflict, "a pending job has nothing to apply")

	require.NoError(t, s.CompleteJob(ctx, job.ID, json.RawMessage(`{}`)))
	assert.ErrorIs(t, consume(newStorage(t, ctx)), store.ErrNotFound, "another storage's job")

	require.NoError(t, consume(storageID))
	assert.Equal(t, "consumed", jobStatus(t, ctx, job.ID))

	assert.ErrorIs(t, consume(storageID), store.ErrConflict, "a proposal cannot be applied twice")
}

// TestRequeueJobAnalysesAFinishedPhotoAgain — "Analyze again" moves a done or
// failed job back to pending with its proposal and error gone, and refuses
// every job that is not finished, has been applied, has no photo, or belongs
// to another storage.
func TestRequeueJobAnalysesAFinishedPhotoAgain(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	photo := uuid.NewString() + ".jpg"

	// The claim of the process running the new analysis. A requeued job is
	// pending again, so it needs an owner for the same reason a new one does.
	reanalysis := store.JobLease{Owner: uuid.Must(uuid.NewV7()), TTL: time.Hour}

	newJob := func(image *string) *store.Job {
		job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion, ImageFilename: image})
		require.NoError(t, err)
		return job
	}

	done := newJob(&photo)
	require.NoError(t, s.CompleteJob(ctx, done.ID, json.RawMessage(`{"rows":[]}`)))
	assert.ErrorIs(t, s.RequeueJob(ctx, newStorage(t, ctx), done.ID, reanalysis), store.ErrNotFound, "another storage's job")
	require.NoError(t, s.RequeueJob(ctx, storageID, done.ID, reanalysis))

	got, err := s.Job(ctx, storageID, done.ID)
	require.NoError(t, err)
	assert.Equal(t, store.JobPending, got.Status)
	assert.Nil(t, got.Payload, "the old proposal must not outlive the new analysis")
	require.NotNil(t, got.ImageFilename)
	assert.Equal(t, photo, *got.ImageFilename, "the photo stays with the job")

	owner, expires := jobClaim(t, ctx, done.ID)
	require.NotNil(t, owner)
	assert.Equal(t, reanalysis.Owner, *owner, "the re-analysis claims the job it just made pending")
	require.NotNil(t, expires)
	assert.True(t, expires.After(time.Now()))

	assert.ErrorIs(t, s.RequeueJob(ctx, storageID, done.ID, reanalysis), store.ErrConflict, "a pending job is already being analysed")

	// A requeued job finishes like a new one.
	require.NoError(t, s.CompleteJob(ctx, done.ID, json.RawMessage(`{"rows":[{"row_id":"0"}]}`)))

	failed := newJob(&photo)
	require.NoError(t, s.FailJob(ctx, failed.ID, "The photo could not be analysed."))
	require.NoError(t, s.RequeueJob(ctx, storageID, failed.ID, reanalysis))
	got, err = s.Job(ctx, storageID, failed.ID)
	require.NoError(t, err)
	assert.Equal(t, store.JobPending, got.Status)
	assert.Nil(t, got.Error, "the old failure must not show while the new analysis runs")

	noPhoto := newJob(nil)
	require.NoError(t, s.CompleteJob(ctx, noPhoto.ID, json.RawMessage(`{"rows":[]}`)))
	assert.ErrorIs(t, s.RequeueJob(ctx, storageID, noPhoto.ID, reanalysis), store.ErrConflict, "nothing to analyse")

	consumed := newJob(&photo)
	require.NoError(t, s.CompleteJob(ctx, consumed.ID, json.RawMessage(`{"rows":[]}`)))
	tx, err := testPool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, store.ConsumeJob(ctx, tx, storageID, consumed.ID))
	require.NoError(t, tx.Commit(ctx))
	assert.ErrorIs(t, s.RequeueJob(ctx, storageID, consumed.ID, reanalysis), store.ErrConflict, "an applied proposal cannot be replaced")
	assert.Equal(t, "consumed", jobStatus(t, ctx, consumed.ID))

	assert.ErrorIs(t, s.RequeueJob(ctx, storageID, uuid.New(), reanalysis), store.ErrNotFound)
}
