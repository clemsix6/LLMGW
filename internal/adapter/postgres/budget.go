package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// rowQuerier is the subset of pgx operations shared by the connection pool and a transaction, so
// the same counter and reservation SQL runs either on the pool (public reads) or inside the
// admission transaction (the concurrency-safe pre-check + reserve).
type rowQuerier interface {
	// QueryRow runs a query expected to return at most one row.
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row

	// Exec runs a statement and returns its command tag.
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// LimitsFor returns the budget limits that apply to a (project, tag): the rows whose tag matches
// the concrete request tag, plus the whole-project rows (tag IS NULL) that apply across every
// tag. Rows are ordered by id so limit evaluation is deterministic (first blocking limit wins).
func (s *Store) LimitsFor(ctx context.Context, projectID int64, tag string) ([]domain.BudgetLimit, error) {
	const query = `
SELECT tag, dimension, "window", max_value, action
FROM budget_limit
WHERE project_id = $1 AND (tag = $2 OR tag IS NULL)
ORDER BY id`

	rows, err := s.pool.Query(ctx, query, projectID, tag)
	if err != nil {
		return nil, fmt.Errorf("query limits for project %d tag %q:\n%w", projectID, tag, err)
	}
	defer rows.Close()

	// A NULL tag scans into a nil *string, preserving the whole-project sentinel for the domain.
	var limits []domain.BudgetLimit

	for rows.Next() {
		var limit domain.BudgetLimit
		if err := rows.Scan(&limit.Tag, &limit.Dimension, &limit.Window, &limit.MaxValue, &limit.Action); err != nil {
			return nil, fmt.Errorf("scan budget limit:\n%w", err)
		}
		limits = append(limits, limit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate budget limits:\n%w", err)
	}
	return limits, nil
}

// Reserve records an in-flight call for a (project, tag) and returns the reservation id. The row
// expires after ttl (computed from the database clock to avoid app/DB skew); InflightTotals
// counts it until then or until ReleaseReservation removes it.
func (s *Store) Reserve(ctx context.Context, projectID int64, tag string, ttl time.Duration) (int64, error) {
	return insertReservation(ctx, s.pool, projectID, tag, ttl)
}

// ReleaseReservation removes a previously created reservation. Releasing an absent id is a no-op.
func (s *Store) ReleaseReservation(ctx context.Context, reservationID int64) error {
	const query = `DELETE FROM reservation WHERE id = $1`

	if _, err := s.pool.Exec(ctx, query, reservationID); err != nil {
		return fmt.Errorf("release reservation %d:\n%w", reservationID, err)
	}
	return nil
}

// InflightTotals aggregates the non-expired reservations for a (project, tag) into a Totals whose
// only populated dimension is Calls — each reservation counts as one in-flight call. Tokens and
// cost are NOT reserved pre-call (output tokens and cost are unknown until the call completes), so
// Calls is the dimension the concurrency guard relies on to stop simultaneous requests from
// collectively overshooting a calls limit. The domain.WholeProjectTag sentinel aggregates across
// every tag of the project, mirroring WindowedTotals.
//
// This method is test-only: production admits through ReserveIfAdmitted, which prunes the
// project's expired reservations under the lock before counting. It is retained for the adapter
// unit tests, so it prunes expired rows first to mirror that production behaviour.
func (s *Store) InflightTotals(ctx context.Context, projectID int64, tag string) (domain.Totals, error) {
	if err := s.pruneExpiredReservations(ctx); err != nil {
		return domain.Totals{}, err
	}
	return inflightCalls(ctx, s.pool, projectID, tag)
}

// ReserveIfAdmitted is the concurrency-safe pre-check + reservation. Within one transaction it
// takes a per-project advisory lock, prunes the project's expired reservations, gathers the
// requested window totals, lets admit rule on them, and — only when admit returns true — inserts
// a reservation, all before the lock releases at commit.
//
// The advisory lock serialises admissions for the SAME project (across every tag): a second
// request for the project blocks on the lock until the first commits, by which point the first's
// reservation row is visible to the second's count. Two concurrent near-limit requests therefore
// cannot both be admitted — including two requests on DIFFERENT tags racing a whole-project
// (tag IS NULL) calls cap, which a per-(project, tag) lock would let overshoot. Different projects
// hash to different locks and never contend.
//
// On a blocking decision the transaction rolls back, including the expired-reservation prune —
// harmless housekeeping (a later admission re-prunes, and the in-flight count already excludes
// expired rows via its expires_at filter). The lock is transaction-scoped, so a blocked request
// leaves no lasting trace.
func (s *Store) ReserveIfAdmitted(ctx context.Context, projectID int64, tag string, ttl time.Duration, reads []domain.WindowRead, admit func(totals []domain.WindowTotals) bool) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin admission for project %d tag %q:\n%w", projectID, tag, err)
	}
	defer tx.Rollback(ctx) // no-op once committed; rolls back a blocked admission.

	if err := lockAdmission(ctx, tx, projectID); err != nil {
		return 0, false, err
	}

	if err := pruneProjectReservations(ctx, tx, projectID); err != nil {
		return 0, false, err
	}

	totals, err := gatherWindowTotals(ctx, tx, projectID, reads)
	if err != nil {
		return 0, false, err
	}

	if !admit(totals) {
		return 0, false, nil
	}

	id, err := insertReservation(ctx, tx, projectID, tag, ttl)
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit admission for project %d tag %q:\n%w", projectID, tag, err)
	}
	return id, true, nil
}

// lockAdmission takes the transaction-scoped advisory lock that serialises admissions for a whole
// project. The project id is a bigint, so it is used directly as the advisory-lock key (no
// hashing, no collisions). The lock auto-releases when the transaction ends.
//
// Keying on the project (not the (project, tag) pair) is what closes the whole-project
// (tag IS NULL) calls-cap overshoot: concurrent requests on different tags can no longer both pass
// a project-wide cap, because the second blocks until the first's reservation is committed and
// visible. The critical section is cheap — a few indexed SELECTs plus one INSERT, no upstream
// call — so per-project serialisation costs little; the slow LLM call happens outside the lock.
func lockAdmission(ctx context.Context, q rowQuerier, projectID int64) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, projectID); err != nil {
		return fmt.Errorf("acquire admission lock for project %d:\n%w", projectID, err)
	}
	return nil
}

// gatherWindowTotals reads the recorded usage and in-flight reservations for each WindowRead,
// preserving the input order so the caller can match each result back to its window's limits.
func gatherWindowTotals(ctx context.Context, q rowQuerier, projectID int64, reads []domain.WindowRead) ([]domain.WindowTotals, error) {
	totals := make([]domain.WindowTotals, len(reads))

	for i, read := range reads {
		current, err := windowedTotals(ctx, q, projectID, read.Tag, read.Since)
		if err != nil {
			return nil, err
		}

		inflight, err := inflightCalls(ctx, q, projectID, read.Tag)
		if err != nil {
			return nil, err
		}
		totals[i] = domain.WindowTotals{Current: current, Inflight: inflight}
	}
	return totals, nil
}

// inflightCalls counts the non-expired reservations for a (project, tag) into Totals.Calls. It
// filters on expires_at without deleting, so the count never includes leaked (expired) rows
// regardless of pruning. In production ReserveIfAdmitted prunes the project's expired rows under
// the admission lock just before this count; deletion is decoupled from counting on purpose.
func inflightCalls(ctx context.Context, q rowQuerier, projectID int64, tag string) (domain.Totals, error) {
	query := `SELECT COUNT(*)::bigint FROM reservation WHERE project_id = $1 AND expires_at >= now()`
	args := []any{projectID}
	if tag != domain.WholeProjectTag {
		query += ` AND tag = $2`
		args = append(args, tag)
	}

	var totals domain.Totals
	if err := q.QueryRow(ctx, query, args...).Scan(&totals.Calls); err != nil {
		return domain.Totals{}, fmt.Errorf("inflight totals for project %d tag %q:\n%w", projectID, tag, err)
	}
	return totals, nil
}

// insertReservation writes one reservation row expiring ttl from the database clock (avoiding
// app/DB skew) and returns its id.
func insertReservation(ctx context.Context, q rowQuerier, projectID int64, tag string, ttl time.Duration) (int64, error) {
	const query = `
INSERT INTO reservation (project_id, tag, expires_at)
VALUES ($1, $2, now() + make_interval(secs => $3))
RETURNING id`

	var id int64
	if err := q.QueryRow(ctx, query, projectID, tag, ttl.Seconds()).Scan(&id); err != nil {
		return 0, fmt.Errorf("reserve for project %d tag %q:\n%w", projectID, tag, err)
	}
	return id, nil
}

// pruneExpiredReservations deletes every reservation whose TTL has elapsed, across all projects.
// It is test-only: it runs before each test-only InflightTotals read so that path mirrors prod.
// Production prunes per-project inside ReserveIfAdmitted (under the admission lock), not here, or
// periodically via PruneOlderThan. It discards the deleted count the retention sweep reports.
func (s *Store) pruneExpiredReservations(ctx context.Context) error {
	_, err := s.pruneExpiredReservationRows(ctx)
	return err
}

// pruneProjectReservations deletes the project's expired reservations. ReserveIfAdmitted calls it
// inside the admission transaction, under the per-project lock, so leaked rows from a request that
// crashed between reserve and release self-heal on the next admission instead of accumulating —
// no separate sweeper needed. The expires_at index keeps the delete cheap as rows pile up.
func pruneProjectReservations(ctx context.Context, q rowQuerier, projectID int64) error {
	const query = `DELETE FROM reservation WHERE project_id = $1 AND expires_at < now()`

	if _, err := q.Exec(ctx, query, projectID); err != nil {
		return fmt.Errorf("prune expired reservations for project %d:\n%w", projectID, err)
	}
	return nil
}
