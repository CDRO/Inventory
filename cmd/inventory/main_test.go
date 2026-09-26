package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/config"
	"github.com/CDRO/Inventory/internal/imagesearch"
	"github.com/CDRO/Inventory/internal/jobs"
	"github.com/CDRO/Inventory/internal/migrate"
	"github.com/CDRO/Inventory/internal/notify"
	"github.com/CDRO/Inventory/internal/store"
)

// TestExitCodeForConfigFailure is the guard on the "fatal, non-retryable exit
// with a distinct code" rule. Docker restart policies do not look at exit
// codes, so this code is what tells an operator reading `docker compose logs
// app` that the container is misconfigured rather than crashing — and a
// regression to a generic 1 would erase that signal silently, with every test
// still green.
func TestExitCodeForConfigFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "missing configuration exits with EX_CONFIG",
			err:  &config.MissingError{Names: []string{"DATABASE_URL"}},
			want: config.ExitConfig,
		},
		{
			name: "a wrapped missing-configuration error still exits with EX_CONFIG",
			err:  fmt.Errorf("starting server: %w", &config.MissingError{Names: []string{"SESSION_SECRET"}}),
			want: config.ExitConfig,
		},
		{
			name: "any other failure exits generically",
			err:  errors.New("dial tcp: connection refused"),
			want: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, exitCodeFor(tc.err))
		})
	}

	assert.Equal(t, 78, config.ExitConfig, "EX_CONFIG is 78; changing it changes an operator-visible contract")
}

// TestErrorMessageKeepsRemediationUnprefixed checks that the remediation block
// reaches the operator intact. Prefixing it would push "No configuration
// found" behind noise on the line they actually read.
func TestErrorMessageKeepsRemediationUnprefixed(t *testing.T) {
	t.Parallel()

	missing := &config.MissingError{Names: []string{"DATABASE_URL"}}

	assert.Equal(t, missing.Error(), errorMessage(missing))
	assert.Equal(t, "inventory: boom", errorMessage(errors.New("boom")))
}

// TestASchemaMismatchIsFatalAndNonRetryable is the acceptance criterion of
// docs/specs/18-operations-and-observability.md: `serve` refuses to start with
// a non-zero exit when migrations are pending, and `restart: unless-stopped`
// does not turn that into log spam.
//
// The distinct exit code is what carries the second half. Docker's restart
// policies do not discriminate on exit codes, so EX_CONFIG is not a signal to
// Docker — it is a signal to the person reading `docker compose logs app`,
// telling them the container is refusing to run rather than crashing, and it
// is the same one the missing-.env check has used since spec 01. The message
// is what names the fix, so it is asserted to arrive unprefixed.
func TestASchemaMismatchIsFatalAndNonRetryable(t *testing.T) {
	t.Parallel()

	behind := &migrate.SchemaMismatchError{DBVersion: 5, BinaryVersion: 10, Pending: 5}
	ahead := &migrate.SchemaMismatchError{DBVersion: 12, BinaryVersion: 10}

	for _, err := range []error{behind, ahead, fmt.Errorf("starting server: %w", behind)} {
		assert.Equal(t, config.ExitConfig, exitCodeFor(err),
			"a schema mismatch exits like a configuration failure: the operator must act")
	}

	assert.Equal(t, behind.Error(), errorMessage(behind),
		"the remediation block reaches the operator unprefixed")
	assert.Contains(t, behind.Error(), "migrate up",
		"and names the operation to apply, though deliberately not a compose invocation")

	// The distinction that makes the code meaningful at all: a database that is
	// merely unreachable is worth retrying, and must not be reported as a
	// deployment the operator has to repair.
	assert.Equal(t, 1, exitCodeFor(errors.New("dial tcp: connection refused")))
}

// TestRunRejectsUnknownCommand keeps a typo from being mistaken for `serve`,
// which would start a server the operator did not ask for.
func TestRunRejectsUnknownCommand(t *testing.T) {
	t.Parallel()

	err := run([]string{"migrat"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown command "migrat"`)
	assert.Equal(t, 1, exitCodeFor(err))
}

// TestStaticFSPrefersStaticDir covers the dev half of the asset switch: with
// STATIC_DIR set the server must read from disk, which is the whole reason
// editing a .js file and refreshing the browser is enough.
func TestStaticFSPrefersStaticDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>from disk</h1>"), 0o600))

	assets, err := staticFS(&config.Config{StaticDir: dir})
	require.NoError(t, err)

	got, err := fs.ReadFile(assets, "index.html")
	require.NoError(t, err)
	assert.Equal(t, "<h1>from disk</h1>", string(got))
}

// TestStaticFSFallsBackToEmbedded covers the production half: an empty
// STATIC_DIR must serve the copy compiled into the binary, because the scratch
// image has no files to mount.
func TestStaticFSFallsBackToEmbedded(t *testing.T) {
	t.Parallel()

	assets, err := staticFS(&config.Config{StaticDir: ""})
	require.NoError(t, err)

	got, err := fs.ReadFile(assets, "index.html")
	require.NoError(t, err, "index.html must be reachable at the root of the embedded tree")
	assert.Contains(t, string(got), "<html", "the embedded asset must be the real page, not an empty file")
}

// TestStaticFSSwitchIsNotInverted asserts the two sources actually differ, so
// an inverted STATIC_DIR check cannot pass both tests above by accident.
func TestStaticFSSwitchIsNotInverted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := "<h1>disk copy, not the embedded one</h1>"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.html"), []byte(marker), 0o600))

	fromDisk, err := staticFS(&config.Config{StaticDir: dir})
	require.NoError(t, err)
	diskBytes, err := fs.ReadFile(fromDisk, "index.html")
	require.NoError(t, err)

	embedded, err := staticFS(&config.Config{})
	require.NoError(t, err)
	embeddedBytes, err := fs.ReadFile(embedded, "index.html")
	require.NoError(t, err)

	assert.Equal(t, marker, string(diskBytes))
	assert.NotEqual(t, string(diskBytes), string(embeddedBytes))
}

// TestNextGamificationRecomputeBeforeTheHourIsToday covers the ordinary case:
// woken up earlier in the day, the next run is later that same day.
func TestNextGamificationRecomputeBeforeTheHourIsToday(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.March, 10, 1, 30, 0, 0, time.UTC)
	next := nextGamificationRecompute(now)

	assert.Equal(t, time.Date(2026, time.March, 10, gamificationRecomputeHour, 0, 0, 0, time.UTC), next)
}

// TestNextGamificationRecomputeAfterTheHourIsTomorrow covers the case that
// would silently recompute twice in one day, or never on time, if the "is it
// still today" comparison were backwards.
func TestNextGamificationRecomputeAfterTheHourIsTomorrow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.March, 10, gamificationRecomputeHour, 0, 1, 0, time.UTC)
	next := nextGamificationRecompute(now)

	assert.Equal(t, time.Date(2026, time.March, 11, gamificationRecomputeHour, 0, 0, 0, time.UTC), next)
}

// TestNextGamificationRecomputeAtExactlyTheHourIsTomorrow: the boundary
// instant must not recompute again immediately — "next" means strictly
// after, not "at or after".
func TestNextGamificationRecomputeAtExactlyTheHourIsTomorrow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.March, 10, gamificationRecomputeHour, 0, 0, 0, time.UTC)
	next := nextGamificationRecompute(now)

	assert.Equal(t, time.Date(2026, time.March, 11, gamificationRecomputeHour, 0, 0, 0, time.UTC), next)
}

// TestNextMondayFromAMidWeekDay covers the ordinary case: woken up any day
// but Monday, the next occurrence is this week's (or next week's) Monday
// 00:00 (docs/specs/52-gamification-quests-and-ui.md: "generated Monday
// 00:00 in the server's local timezone").
func TestNextMondayFromAMidWeekDay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.March, 11, 15, 0, 0, 0, time.UTC) // a Wednesday
	next := nextMonday(now)

	assert.Equal(t, time.Date(2026, time.March, 16, 0, 0, 0, 0, time.UTC), next) // the following Monday
	assert.Equal(t, time.Monday, next.Weekday())
}

// TestNextMondayOnMondayBeforeMidnightIsToday covers being woken very early
// on a Monday, before 00:00 has technically arrived for the process's clock
// — the next occurrence is still today.
func TestNextMondayOnMondayBeforeMidnightIsToday(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC).Add(-time.Nanosecond) // one ns before Monday 00:00
	next := nextMonday(now)

	assert.Equal(t, time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC), next)
}

// TestNextMondayOnMondayAfterMidnightIsNextWeek: the boundary instant and
// anything after it on a Monday must not fire again immediately — "next"
// means strictly after, not "at or after", the same rule
// nextGamificationRecompute follows.
func TestNextMondayOnMondayAfterMidnightIsNextWeek(t *testing.T) {
	t.Parallel()

	cases := []time.Time{
		time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC),  // exactly at the boundary
		time.Date(2026, time.March, 9, 12, 0, 0, 0, time.UTC), // later the same Monday
	}
	for _, now := range cases {
		next := nextMonday(now)
		assert.Equal(t, time.Date(2026, time.March, 16, 0, 0, 0, 0, time.UTC), next, "now=%s", now)
	}
}

// discardLogger is a *slog.Logger that never writes anywhere, for
// constructors below that require one.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCacheStore satisfies imagesearch.CacheStore without a database, and
// counts calls so a test can tell imageCache.RunSweeps actually reached the
// store rather than merely returning without panicking — the same thing a
// no-op closure of the same name would also do.
type fakeCacheStore struct{ calls atomic.Int32 }

func (f *fakeCacheStore) CachedImageByHash(context.Context, string) (*store.CachedImage, error) {
	return nil, nil
}
func (f *fakeCacheStore) PutCachedImage(context.Context, store.CachedImage) error { return nil }
func (f *fakeCacheStore) TouchCachedImage(context.Context, string, time.Duration) error {
	return nil
}
func (f *fakeCacheStore) CachedImageBytes(context.Context) (int64, error) {
	f.calls.Add(1)
	return 0, nil
}
func (f *fakeCacheStore) LeastRecentlyUsedImages(context.Context, int) ([]store.CachedImage, error) {
	return nil, nil
}
func (f *fakeCacheStore) DeleteCachedImage(context.Context, string) error { return nil }
func (f *fakeCacheStore) CachedImageHashes(context.Context) (map[string]struct{}, error) {
	f.calls.Add(1)
	return map[string]struct{}{}, nil
}

// fakeJobsStore satisfies jobs.Store without a database, so
// TestBackgroundLoopsCoversAllSix can enter and stop jobRunner.RunLeases for
// real.
type fakeJobsStore struct{}

func (fakeJobsStore) CreateJob(context.Context, store.NewJob) (*store.Job, error) { return nil, nil }
func (fakeJobsStore) RequeueJob(context.Context, uuid.UUID, uuid.UUID, store.JobLease) error {
	return nil
}
func (fakeJobsStore) CompleteJob(context.Context, uuid.UUID, json.RawMessage) error { return nil }
func (fakeJobsStore) FailJob(context.Context, uuid.UUID, string) error              { return nil }
func (fakeJobsStore) FailOrphanedJobs(context.Context, uuid.UUID) (int64, error)    { return 0, nil }
func (fakeJobsStore) RenewJobLeases(context.Context, store.JobLease) (int64, error) { return 0, nil }
func (fakeJobsStore) ReleaseJobLeases(context.Context, uuid.UUID) (int64, error)    { return 0, nil }

// fakeSweepStore satisfies storeSweeper without a database, and counts calls
// so a test can tell runStoreSweeps actually reached them rather than merely
// returning without panicking.
type fakeSweepStore struct{ calls atomic.Int32 }

func (f *fakeSweepStore) SweepSessions(context.Context) (int64, error) {
	f.calls.Add(1)
	return 0, nil
}
func (f *fakeSweepStore) SweepPairingCodes(context.Context) (int64, error) {
	f.calls.Add(1)
	return 0, nil
}
func (f *fakeSweepStore) SweepIdempotencyRecords(context.Context, time.Time) (int64, error) {
	f.calls.Add(1)
	return 0, nil
}
func (f *fakeSweepStore) SweepTombstones(context.Context, time.Time) (int64, error) {
	f.calls.Add(1)
	return 0, nil
}

// fakeNotifyStore satisfies notify.Store without a database. Its methods are
// never expected to run in the tests below, since runExpiryNotifications
// checks ctx.Done() before ever reaching them — proving that loop was
// entered relies on assertLoopBlocksUntilCancelled instead, and this fake
// exists only so notify.New has something to hold.
type fakeNotifyStore struct{}

func (fakeNotifyStore) ClaimDueNotifications(context.Context, time.Time, time.Time) ([]store.NotificationSettings, error) {
	return nil, nil
}
func (fakeNotifyStore) ExpiringBatchesBefore(context.Context, uuid.UUID, time.Time) ([]store.DigestItem, error) {
	return nil, nil
}
func (fakeNotifyStore) RecordNotificationResult(context.Context, uuid.UUID, string) error {
	return nil
}

// TestStartBackgroundLoopsEntersEveryLoop is the generic half of issue #214's
// fix: the launcher itself must start every loop handed to it, even one whose
// own body only checks ctx.Done() and would otherwise look skipped rather
// than merely quick to return.
func TestStartBackgroundLoopsEntersEveryLoop(t *testing.T) {
	t.Parallel()

	const n = 6
	var wg sync.WaitGroup
	wg.Add(n)
	loops := make([]backgroundLoop, n)
	for i := range loops {
		loops[i] = backgroundLoop{
			name: fmt.Sprintf("loop-%d", i),
			run:  func(context.Context) { wg.Done() },
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	startBackgroundLoops(ctx, loops)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("startBackgroundLoops did not enter every loop")
	}
}

// assertLoopBlocksUntilCancelled proves a loop that only checks ctx.Done()
// before ever touching its dependencies (nightly gamification recompute,
// weekly gamification jobs, expiry notifications) actually entered its own
// body and is blocked waiting on the next scheduled run, rather than being a
// no-op that happens to share its name and would also return instantly under
// a context that is already cancelled.
//
// Given a live, uncancelled context it must still be running after a short
// pause — the discriminating signal, since a no-op returns before the pause
// ever starts — and only once cancelled does it stop.
func assertLoopBlocksUntilCancelled(t *testing.T, name string, run func(context.Context)) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		run(ctx)
		close(done)
	}()

	select {
	case <-done:
		t.Errorf("%s returned without ever waiting on its context — a no-op with the same name would do exactly this", name)
		return
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("%s did not stop after its context was cancelled", name)
	}
}

// TestBackgroundLoopsCoversAllSix is the concrete half of issue #214's fix:
// serve() must wire up exactly the six loops the issue names, and each one
// must actually be reachable when started — not just present as a line of
// code. Deleting any entry from backgroundLoops, or breaking how one of them
// enters, fails this test; each loop's own package tests call it directly and
// so cannot notice serve() failing to start it.
func TestBackgroundLoopsCoversAllSix(t *testing.T) {
	t.Parallel()

	log := discardLogger()
	cacheStore := &fakeCacheStore{}
	imageCache := imagesearch.NewCache(t.TempDir(), cacheStore, nil, log)
	jobRunner := jobs.New(fakeJobsStore{}, log)
	notifier := notify.New(fakeNotifyStore{}, log)
	sweepDB := &fakeSweepStore{}

	// gamificationDB is nil on purpose: runGamificationRecompute and
	// runWeeklyGamificationJobs both check ctx.Done() before ever touching
	// their *store.Store argument, so entering them below never dereferences
	// it, whether the context handed to them is cancelled or still live.
	loops := backgroundLoops(imageCache, jobRunner, sweepDB, nil, nil, notifier)

	wantNames := []string{
		"image suggestion cache sweep",
		"job lease keeper",
		"store retention sweep",
		"nightly gamification recompute",
		"weekly gamification jobs",
		"expiry notifications",
	}
	gotNames := make([]string, len(loops))
	byName := make(map[string]func(context.Context), len(loops))
	for i, l := range loops {
		gotNames[i] = l.name
		byName[l.name] = l.run
	}
	assert.ElementsMatch(t, wantNames, gotNames,
		"serve() must start all six loops issue #214 names — deleting one keeps `go test ./...` green everywhere else")

	// The two sweeps do real, unconditional work the instant they are
	// entered — sweep() runs once before either ever looks at ctx — so a
	// cancelled context is enough to prove both "entered" (via the store
	// call counters below) and "does not hang or panic on shutdown".
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	var wg sync.WaitGroup
	for _, name := range []string{"image suggestion cache sweep", "store retention sweep"} {
		run := byName[name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NotPanics(t, func() { run(cancelledCtx) }, "%s must be safely enterable", name)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a sweep did not return promptly for a cancelled context")
	}
	assert.GreaterOrEqual(t, cacheStore.calls.Load(), int32(1),
		"image suggestion cache sweep must actually reach the store, not just return")
	assert.GreaterOrEqual(t, sweepDB.calls.Load(), int32(1),
		"store retention sweep must actually reach the store, not just return")

	// The remaining three all check ctx.Done() before touching anything, so
	// an already-cancelled context would make even a no-op indistinguishable
	// from the real loop; assertLoopBlocksUntilCancelled proves entry instead
	// by requiring the real one to still be running a moment later.
	for _, name := range []string{"nightly gamification recompute", "weekly gamification jobs", "expiry notifications"} {
		assertLoopBlocksUntilCancelled(t, name, byName[name])
	}

	// The lease keeper is the one loop that ignores the context it is handed
	// entirely (issue #121/#219) and stops only when the runner itself shuts
	// down, so it needs its own proof: still running just before Shutdown —
	// ruling out a no-op that would have already returned — and stopped
	// shortly after it.
	leaseRun := byName["job lease keeper"]
	require.NotNil(t, leaseRun)

	leaseDone := make(chan struct{})
	go func() {
		leaseRun(context.Background())
		close(leaseDone)
	}()
	select {
	case <-leaseDone:
		t.Fatal("job lease keeper returned before the runner was ever shut down — a no-op would do exactly this")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, jobRunner.Shutdown(context.Background()))
	select {
	case <-leaseDone:
	case <-time.After(5 * time.Second):
		t.Fatal("job lease keeper did not stop after the runner shut down")
	}
}
