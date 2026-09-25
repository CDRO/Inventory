package store_test

import (
	"context"
	"testing"
	"time"
)

// waitForBackendBlockedOn polls pg_stat_activity until some backend's current
// query contains substr and that backend is waiting on a lock. Tests use it
// to prove a concurrent statement has actually reached the point they mean to
// interleave at, rather than racing it with a fixed sleep — which would
// either flake under load or waste a fixed delay every other time.
//
// This is deliberately narrow, not a general transaction-interleaving
// framework. What it gives: given two goroutines each driving their own
// connection, one holding a row lock (e.g. SELECT ... FOR UPDATE) and the
// other attempting a statement that needs the same lock, this lets the test
// wait for the second to actually block before it lets the first proceed —
// see TestConcurrentDeleteLosesTheAuditRace in audit_test.go. What it does
// not give: isolation from unrelated activity in the same database (the
// query filters by substr for that reason, but a large enough test suite
// running in parallel could still coincide), and it only detects
// lock waits (wait_event_type = 'Lock') — a synchronization point that
// isn't backed by a real row/table lock won't show up here at all.
func waitForBackendBlockedOn(t *testing.T, ctx context.Context, substr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		n := countRows(t, ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND query ILIKE $1`, "%"+substr+"%")
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for a backend to block on a query containing %q", timeout, substr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
