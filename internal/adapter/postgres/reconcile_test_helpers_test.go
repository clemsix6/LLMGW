package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// reconcileOutcome carries one background reconciliation result through a bounded test channel.
type reconcileOutcome struct {
	result governance.ReconcileResult // result is the aggregate state transition count.
	err    error                      // err is the reconciliation failure, if any.
}

// installObservedUpdateBarrier pauses RecordAttempt after it has locked and inserted its parent attempt.
func installObservedUpdateBarrier(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	const query = `
CREATE FUNCTION block_observed_parent_update() RETURNS trigger AS $$
BEGIN
    IF OLD.state = 'in_flight' AND NEW.accounting_state = 'observed' THEN
        PERFORM pg_advisory_xact_lock(808080);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER block_observed_parent_update
BEFORE UPDATE ON request_event
FOR EACH ROW EXECUTE FUNCTION block_observed_parent_update()`
	if _, err := store.pool.Exec(ctx, query); err != nil {
		t.Fatalf("install observed update barrier: %v", err)
	}
}

// holdObservedUpdateBarrier acquires the test advisory lock and returns its idempotent release.
func holdObservedUpdateBarrier(t *testing.T, ctx context.Context, store *Store) func() {
	t.Helper()
	connection, err := store.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire barrier connection: %v", err)
	}
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(808080)`); err != nil {
		connection.Release()
		t.Fatalf("hold observed update barrier: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, err := connection.Exec(context.Background(), `SELECT pg_advisory_unlock(808080)`); err != nil {
			t.Errorf("release observed update barrier: %v", err)
		}
		connection.Release()
	}
	t.Cleanup(release)
	return release
}

// waitForDatabaseWait confirms the intended PostgreSQL interleaving before releasing the barrier.
func waitForDatabaseWait(t *testing.T, ctx context.Context, store *Store, waitEvent string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		const query = `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity
    WHERE datname = current_database()
      AND pid <> pg_backend_pid()
      AND wait_event_type = 'Lock'
      AND wait_event = $1
)`
		if err := store.pool.QueryRow(ctx, query, waitEvent).Scan(&waiting); err != nil {
			t.Fatalf("inspect database wait %q: %v", waitEvent, err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("database did not enter %q lock wait", waitEvent)
}

// waitForAttempt returns a bounded attempt result.
func waitForAttempt(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("RecordAttempt did not finish")
		return nil
	}
}

// waitForReconcile returns a bounded reconciliation result.
func waitForReconcile(t *testing.T, done <-chan reconcileOutcome) reconcileOutcome {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-time.After(2 * time.Second):
		t.Fatal("ReconcileAccounting did not finish")
		return reconcileOutcome{}
	}
}

// usageAttemptSnapshot returns one request's complete durable attempt rows as stable JSON.
func usageAttemptSnapshot(t *testing.T, ctx context.Context, store *Store, requestID string) string {
	t.Helper()
	var snapshot string
	const query = `
SELECT COALESCE(json_agg(row_to_json(a) ORDER BY a.id)::text, '[]')
FROM usage_attempt a
WHERE a.request_id = $1`
	if err := store.pool.QueryRow(ctx, query, requestID).Scan(&snapshot); err != nil {
		t.Fatalf("snapshot usage attempts: %v", err)
	}
	return snapshot
}
