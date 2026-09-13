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
	CreateJob(ctx context.Context, storageID uuid.UUID, kind store.JobKind, createdBy *uuid.UUID) (*store.Job, error)
	CompleteJob(ctx context.Context, id uuid.UUID, payload json.RawMessage) error
	FailJob(ctx context.Context, id uuid.UUID, message string) error
	FailInterruptedJobs(ctx context.Context) (int64, error)
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

// New builds a Runner. Call Recover once before the server accepts requests.
func New(s Store, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	base, cancel := context.WithCancel(context.Background())
	return &Runner{
		store:   s,
		log:     log,
		timeout: DefaultTimeout,
		base:    base,
		cancel:  cancel,
		slots:   make(chan struct{}, DefaultConcurrency),
	}
}

// Recover fails every job the previous process left pending.
//
// It must run before Submit is first called: at that point no goroutine of
// this process can own a pending row, so every one of them is orphaned.
func (r *Runner) Recover(ctx context.Context) error {
	n, err := r.store.FailInterruptedJobs(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		r.log.Info("marked interrupted jobs failed", slog.Int64("count", n))
	}
	return nil
}

// Submit records a pending job and starts its work in the background.
//
// ctx is the request's and is used only for the insert; the work runs on the
// runner's own context, because the request that submitted it is about to
// return 202 and end.
func (r *Runner) Submit(ctx context.Context, storageID uuid.UUID, kind store.JobKind, createdBy *uuid.UUID, work Work) (*store.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrShutDown
	}

	job, err := r.store.CreateJob(ctx, storageID, kind, createdBy)
	if err != nil {
		return nil, err
	}

	r.wg.Add(1)
	go r.run(job.ID, work)
	return job, nil
}

// ErrShutDown is Submit after Shutdown.
var ErrShutDown = errors.New("jobs: runner is shut down")

// Shutdown cancels in-flight work and waits for every job to record its
// outcome, or for ctx to end.
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
		return nil
	case <-ctx.Done():
		// Whatever is still running stays pending, and the next Recover
		// fails it. Nothing is left hanging either way.
		return ctx.Err()
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
