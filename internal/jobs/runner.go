// Package jobs runs slow work — vision calls — off the request path
// (docs/specs/04-backend-api-conventions.md).
//
// There is no queue broker. A household server does a handful of these a day,
// so a goroutine per job plus the jobs table is enough, and the table is what
// makes the state survive a restart visibly: a job is always pending, done,
// failed or consumed, never silently lost.
//
// The shape a caller sees is: persist the input, call Submit with the work,
// return 202 with the job id. Everything after that — the result, the failure,
// a server restart mid-flight — is recorded on the row, where the review inbox
// and the polling helper read it.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// Defaults for a Runner.
const (
	// DefaultConcurrency bounds how many jobs call out at once. A user
	// uploading six shelf photos in one go gets them processed a few at a time
	// rather than as six simultaneous calls to a rate-limited provider.
	DefaultConcurrency = 3

	// DefaultTimeout bounds one job. A vision call that has not answered in
	// this long is not going to, and a pending job that never ends is exactly
	// what the jobs table exists to prevent.
	DefaultTimeout = 2 * time.Minute

	// DefaultLeaseTTL is how long this process's claim on a pending job stands
	// without being renewed (issue #121). It is the only knob in the trade the
	// lease makes: a shorter TTL fails a hard-killed process's jobs sooner, a
	// longer one is more forgiving of a database stall that delays a renewal in
	// a process that is perfectly alive.
	//
	// It has deliberately nothing to do with DefaultTimeout. That bounds a
	// job's *execution* and starts only once the job has a concurrency slot, so
	// a queued job is legitimately pending for far longer — which is why an age
	// bound cannot stand in for an owner.
	DefaultLeaseTTL = time.Minute

	// defaultLeaseInterval is how often the lease keeper renews this process's
	// claims and fails everyone else's lapsed ones. Three renewals per TTL, so
	// two that do not get through — a slow query, a moment of database trouble —
	// do not make a live instance look dead to another one.
	defaultLeaseInterval = DefaultLeaseTTL / 3

	// finishTimeout bounds the final write of a job's outcome. It runs on a
	// context detached from the job's own, so a job cancelled by shutdown can
	// still record that it was interrupted.
	finishTimeout = 10 * time.Second
)

// GenericFailure is what the user reads when work fails for a reason that was
// not written for them. The real error goes to the log.
const GenericFailure = "Processing failed. Try uploading the photo again."

// TimeoutFailure is what the user reads when work outlived its timeout.
const TimeoutFailure = "Processing took too long. Try uploading the photo again."

// Store is the slice of the store the runner writes.
type Store interface {
	CreateJob(ctx context.Context, in store.NewJob) (*store.Job, error)
	RequeueJob(ctx context.Context, storageID, id uuid.UUID, lease store.JobLease) error
	CompleteJob(ctx context.Context, id uuid.UUID, payload json.RawMessage) error
	FailJob(ctx context.Context, id uuid.UUID, message string) error
	FailOrphanedJobs(ctx context.Context, owner uuid.UUID) (int64, error)
	RenewJobLeases(ctx context.Context, lease store.JobLease) (int64, error)
	ReleaseJobLeases(ctx context.Context, owner uuid.UUID) (int64, error)
}

// Work does one job and returns its proposal.
//
// It must respect ctx: cancellation means the job timed out or the server is
// shutting down.
type Work func(ctx context.Context) (json.RawMessage, error)

// UserError is a failure whose message is written for the person reviewing the
// job, such as "the photo was too dark to read". Anything else is reported as
// GenericFailure, so a provider's error text — status codes, request ids, an
// API key's name — never reaches a review inbox.
type UserError struct {
	Message string
	Err     error
}

func (e *UserError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *UserError) Unwrap() error { return e.Err }

// Runner starts jobs and records how they end.
type Runner struct {
	store   Store
	log     *slog.Logger
	timeout time.Duration

	// owner identifies this process among however many are running against the
	// same database — during the rolling update on the NAS, two of them
	// (deploy/synology/update). Every job this runner starts is claimed in its
	// name, and recovery in another process leaves those rows alone for as long
	// as the claim is renewed.
	owner    uuid.UUID
	leaseTTL time.Duration
	// leaseInterval is defaultLeaseInterval, overridden by tests.
	leaseInterval time.Duration

	// base is the parent of every job's context. Cancelling it is how Shutdown
	// tells in-flight work to stop.
	base   context.Context
	cancel context.CancelFunc

	slots chan struct{}

	// mu guards closed together with wg.Add, so a Submit racing Shutdown
	// either starts before Shutdown waits or is refused — never a WaitGroup
	// Add concurrent with its Wait.
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// New builds a Runner. Call Recover once before the server accepts requests,
// and run RunLeases for as long as the process serves.
//
// The lease owner is minted here, per process, with uuid.New rather than the
// uuid.NewV7 the rest of the system uses for stored ids: this one is not an
// entity id — never returned by the API, never a key, never ordered on — so it
// has no use for a time prefix.
//
// uuid.New is Must(NewRandom()), so it panics where every other reader of
// crypto/rand in this repository returns an error (store.NewToken,
// auth.HashPassword's salt). That is the deliberate part, and it is not an
// appeal to those call sites: it is that they have somewhere to degrade to and
// this does not. A token that cannot be minted fails one request; a runner with
// no owner would read every pending row — including the ones it is working
// itself — as an orphan, and fail live work on every tick. Start-up is the right
// place for that to stop, and on the Linux/scratch target crypto/rand blocks
// rather than failing anyway.
func New(s Store, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	base, cancel := context.WithCancel(context.Background())
	return &Runner{
		store:         s,
		log:           log,
		timeout:       DefaultTimeout,
		owner:         uuid.New(),
		leaseTTL:      DefaultLeaseTTL,
		leaseInterval: defaultLeaseInterval,
		base:          base,
		cancel:        cancel,
		slots:         make(chan struct{}, DefaultConcurrency),
	}
}

// lease is the claim this runner puts on every job it starts.
func (r *Runner) lease() store.JobLease {
	return store.JobLease{Owner: r.owner, TTL: r.leaseTTL}
}

// Recover fails every pending job whose owner is gone.
//
// Before the lease existed this failed every pending row, on the reasoning that
// it runs before Submit and so no goroutine of this process can own one. That
// reasoning was never about *this* process: it silently assumed there is only
// ever one, which the rolling update on the NAS breaks on purpose by starting a
// second instance next to the first (deploy/synology/update, issue #121). What
// it now fails is a row nobody claims and a row whose claim has lapsed — a
// process that stopped renewing — and it leaves alone the pending work of an
// instance that is demonstrably still alive.
//
// One statement against a partial index, as before: it runs before the listener
// and must not be what delays it.
func (r *Runner) Recover(ctx context.Context) error {
	r.log.Info("job lease owner", slog.String("owner", r.owner.String()))
	n, err := r.store.FailOrphanedJobs(ctx, r.owner)
	if err != nil {
		return err
	}
	if n > 0 {
		r.log.Info("marked interrupted jobs failed", slog.Int64("count", n))
	}
	return nil
}

// RunLeases keeps this process's claims current and fails the ones nobody is
// keeping current any more, once per lease interval until the runner is shut
// down. Run it in a goroutine for the life of the process.
//
// ctx is the context of the lease statements themselves. It deliberately does
// not stop the loop, so it is safe — and in cmd/inventory correct — to hand
// this a context that outlives the process's shutdown signal. Only Shutdown
// stops the keeper; the select below says why that distinction is the whole
// point of the feature.
//
// The two halves are the same tick on purpose. Renewing is what tells other
// instances that this one's pending jobs — running and queued alike — are still
// being worked; sweeping is what fails a job whose owner died without being
// replaced, which start-up recovery alone cannot do. An instance that is
// force-removed while the other keeps running (issue #124, item 4) leaves rows
// no start-up will ever look at again, and before this they stayed pending for
// good.
func (r *Runner) RunLeases(ctx context.Context) {
	ticker := time.NewTicker(r.leaseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.base.Done():
			// The only way out, and deliberately not ctx.Done(). Shutdown
			// cancels base first, so the keeper stops before Shutdown releases
			// the claims it is holding, rather than renewing one back to life a
			// moment afterwards.
			//
			// Stopping on ctx as well would end the keeper when the process is
			// *asked* to stop rather than when it actually stops working:
			// cmd/inventory's root context is cancelled the instant SIGTERM
			// arrives, and the in-flight jobs keep running for up to the
			// shutdown grace after that. A claim that stops being renewed while
			// its work is still running is precisely what lets the instance
			// starting up beside this one fail that job out from under it —
			// issue #121, on the ordinary rolling-update path rather than the
			// killed-outright one.
			return
		case <-ticker.C:
			if n, err := r.store.RenewJobLeases(ctx, r.lease()); err != nil {
				if ctx.Err() == nil {
					r.log.Warn("renewing job leases failed", slog.Any("err", err))
				}
			} else if n > 0 {
				r.log.Debug("renewed job leases", slog.Int64("count", n))
			}

			if n, err := r.store.FailOrphanedJobs(ctx, r.owner); err != nil {
				if ctx.Err() == nil {
					r.log.Warn("sweeping orphaned jobs failed", slog.Any("err", err))
				}
			} else if n > 0 {
				r.log.Info("marked interrupted jobs failed", slog.Int64("count", n))
			}
		}
	}
}

// Submit records a pending job and starts its work in the background.
//
// ctx is the request's and is used only for the insert; the work runs on the
// runner's own context, because the request that submitted it is about to
// return 202 and end.
func (r *Runner) Submit(ctx context.Context, in store.NewJob, work Work) (*store.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrShutDown
	}

	// The claim is this runner's, whatever the caller built: it is the process
	// that is about to work the job, and it is written by the insert itself so
	// the row is never pending and unowned.
	in.Lease = r.lease()
	job, err := r.store.CreateJob(ctx, in)
	if err != nil {
		return nil, err
	}

	r.wg.Add(1)
	go r.run(job.ID, work)
	return job, nil
}

// Resubmit moves a finished job back to pending and starts its work again —
// "Analyze again" (docs/specs/09-consumption-logging.md).
//
// The store decides whether the job may move: only a done or failed job with a
// photo does, and anything else comes back as its ErrConflict or ErrNotFound
// with no work started. Once it has moved, the job ends the way a new one does,
// so a re-analysis that fails, times out or is interrupted by a restart is
// recorded on the row just the same.
func (r *Runner) Resubmit(ctx context.Context, storageID, id uuid.UUID, work Work) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrShutDown
	}

	if err := r.store.RequeueJob(ctx, storageID, id, r.lease()); err != nil {
		return err
	}

	r.wg.Add(1)
	go r.run(id, work)
	return nil
}

// ErrShutDown is Submit after Shutdown.
var ErrShutDown = errors.New("jobs: runner is shut down")

// Shutdown cancels in-flight work and waits for every job to record its
// outcome, or for ctx to end. Either way it gives up this process's claims
// before it returns.
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		r.releaseLeases()
		return nil
	case <-ctx.Done():
		// Whatever is still running stays pending, and releasing the claim is
		// what lets the next Recover fail it straight away instead of waiting
		// out a lease nobody will renew. Nothing is left hanging either way.
		r.releaseLeases()
		return ctx.Err()
	}
}

// releaseLeases gives up this process's claims on whatever is still pending.
//
// On its own context, like finish: it runs when the shutdown deadline has
// usually already passed, and it is precisely then that there is something left
// to release. It is best-effort — a process that is killed outright never gets
// here, which is what the timed sweep in RunLeases covers.
func (r *Runner) releaseLeases() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.base), finishTimeout)
	defer cancel()

	n, err := r.store.ReleaseJobLeases(ctx, r.owner)
	if err != nil {
		r.log.Warn("releasing job leases failed", slog.Any("err", err))
		return
	}
	if n > 0 {
		r.log.Info("released job leases still held at shutdown", slog.Int64("count", n))
	}
}

func (r *Runner) run(id uuid.UUID, work Work) {
	defer r.wg.Done()

	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-r.base.Done():
		r.finish(id, nil, context.Canceled)
		return
	}

	ctx, cancel := context.WithTimeout(r.base, r.timeout)
	defer cancel()

	payload, err := r.call(ctx, work)
	r.finish(id, payload, err)
}

// call runs the work, turning a panic into an error. One bad photo must fail
// its own job, not take down the server and every other job with it.
func (r *Runner) call(ctx context.Context, work Work) (payload json.RawMessage, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("jobs: work panicked: %v", p)
		}
	}()
	return work(ctx)
}

func (r *Runner) finish(id uuid.UUID, payload json.RawMessage, workErr error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.base), finishTimeout)
	defer cancel()

	var err error
	switch {
	case workErr == nil:
		if len(payload) == 0 || !json.Valid(payload) {
			r.log.Error("job produced an invalid payload", slog.String("job_id", id.String()))
			err = r.store.FailJob(ctx, id, GenericFailure)
		} else {
			err = r.store.CompleteJob(ctx, id, payload)
		}
	case r.base.Err() != nil:
		// Shutdown, not a failure of the work: tell the user what the restart
		// sweep would have told them.
		err = r.store.FailJob(ctx, id, store.InterruptedJobError)
	case errors.Is(workErr, context.DeadlineExceeded):
		r.log.Warn("job timed out", slog.String("job_id", id.String()), slog.Any("err", workErr))
		err = r.store.FailJob(ctx, id, TimeoutFailure)
	default:
		message := GenericFailure
		var userErr *UserError
		if errors.As(workErr, &userErr) {
			message = userErr.Message
		}
		r.log.Warn("job failed", slog.String("job_id", id.String()), slog.Any("err", workErr))
		err = r.store.FailJob(ctx, id, message)
	}

	if errors.Is(err, store.ErrNotFound) {
		// Discarded while running. Nobody is waiting for this result.
		return
	}
	if err != nil {
		r.log.Error("recording job outcome failed", slog.String("job_id", id.String()), slog.Any("err", err))
	}
}
