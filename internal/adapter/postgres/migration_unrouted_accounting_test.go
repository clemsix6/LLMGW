package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// unroutedBackfillCase is one seeded request the backfill either resolves or
// must leave exactly where it is.
type unroutedBackfillCase struct {
	id         string // id is the request UUID.
	path       string // path is the requested path.
	status     *int   // status is the downstream status, nil when none was recorded.
	accounting string // accounting is the seeded accounting state.
	attempt    bool   // attempt seeds one durable usage attempt for the request.
	want       string // want is the accounting state the backfill must leave behind.
}

// TestUnroutedBackfillResolvesOnlyCertainZeros replays the backfill over one
// row of every shape the table holds. The rows it resolves are the ones the
// gateway answered from its own router, which reach no provider and can owe
// nothing. Every other row must survive untouched: accounting_unknown means
// "a provider may have charged us and we cannot tell", and a backfill that
// erases that meaning would silently unblock budgets that are blocking for a
// reason.
func TestUnroutedBackfillResolvesOnlyCertainZeros(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	requestedAt := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)

	key, err := store.CreateKey(ctx, "backfill-project", "backfill-key", "pk-backfill", make([]byte, 32), nil)
	if err != nil {
		t.Fatalf("create project key: %v", err)
	}

	notFound, tooManyRequests, success := 404, 429, 200
	cases := []unroutedBackfillCase{
		{
			id: "00000000-0000-0000-0000-000000000001", path: "/api/tags",
			status: &notFound, accounting: "accounting_unknown", want: "resolved_zero",
		},
		{
			id: "00000000-0000-0000-0000-000000000002", path: "/v1/models/claude-opus-5",
			status: &notFound, accounting: "pending", want: "resolved_zero",
		},
		{
			id: "00000000-0000-0000-0000-000000000003", path: "/v1/messages",
			status: &notFound, accounting: "accounting_unknown", want: "accounting_unknown",
		},
		{
			id: "00000000-0000-0000-0000-000000000004", path: "/v1beta/models/gemini-3:generateContent",
			status: &notFound, accounting: "accounting_unknown", want: "accounting_unknown",
		},
		{
			id: "00000000-0000-0000-0000-000000000005", path: "/v1/messages",
			status: &success, accounting: "accounting_unknown", want: "accounting_unknown",
		},
		{
			id: "00000000-0000-0000-0000-000000000006", path: "/v1/messages",
			status: nil, accounting: "accounting_unknown", want: "accounting_unknown",
		},
		{
			id: "00000000-0000-0000-0000-000000000007", path: "/v1/messages",
			status: &tooManyRequests, accounting: "accounting_unknown", want: "accounting_unknown",
		},
		{
			id: "00000000-0000-0000-0000-000000000008", path: "/api/show",
			status: &notFound, accounting: "accounting_unknown", attempt: true,
			want: "accounting_unknown",
		},
	}

	for index, seeded := range cases {
		insertCompletedRequest(t, ctx, store, key.ProjectID, key.ID, seeded, requestedAt)
		if seeded.attempt {
			insertDurableAttempt(t, ctx, store, attemptIDFor(index), seeded.id, requestedAt)
		}
	}

	replayMigration(t, ctx, store, "migrations/0021_resolve_unrouted_request_accounting.sql")

	for _, seeded := range cases {
		if got := accountingStateOf(t, ctx, store, seeded.id); got != seeded.want {
			t.Errorf("%s (%s) resolved to %q, want %q", seeded.path, seeded.accounting, got, seeded.want)
		}
	}
}

// insertCompletedRequest seeds one completed request in a chosen accounting state.
func insertCompletedRequest(
	t *testing.T,
	ctx context.Context,
	store *Store,
	projectID int64,
	clientKeyID int64,
	seeded unroutedBackfillCase,
	requestedAt time.Time,
) {
	t.Helper()
	const query = `
INSERT INTO request_event (
    id, project_id, client_key_id, operation, requested_at, completed_at,
    method, path, state, accounting_state, downstream_status
) VALUES ($1, $2, $3, 'generation', $4, $4, 'GET', $5, 'completed', $6, $7)`
	if _, err := store.pool.Exec(
		ctx, query,
		seeded.id, projectID, clientKeyID, requestedAt,
		seeded.path, seeded.accounting, seeded.status,
	); err != nil {
		t.Fatalf("insert request %q: %v", seeded.id, err)
	}
}

// replayMigration executes one embedded migration against an existing database.
func replayMigration(t *testing.T, ctx context.Context, store *Store, name string) {
	t.Helper()
	statements, err := migrationsFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := store.pool.Exec(ctx, string(statements)); err != nil {
		t.Fatalf("replay %s: %v", name, err)
	}
}

// accountingStateOf reads one request's accounting state.
func accountingStateOf(t *testing.T, ctx context.Context, store *Store, requestID string) string {
	t.Helper()
	var state string
	const query = `SELECT accounting_state FROM request_event WHERE id = $1`
	if err := store.pool.QueryRow(ctx, query, requestID).Scan(&state); err != nil {
		t.Fatalf("read request %q: %v", requestID, err)
	}
	return state
}

// attemptIDFor derives one stable attempt UUID from a seeded row's position.
func attemptIDFor(index int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-0000000001%02d", index)
}
