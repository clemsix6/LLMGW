package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// newTestStore starts an ephemeral Postgres, applies migrations, and returns a ready Store.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	dsn := startTestPostgres(t, ctx)

	store, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// startTestPostgres runs an ephemeral Postgres container and returns its DSN.
func startTestPostgres(t *testing.T, ctx context.Context) string {
	t.Helper()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("llmgw"),
		tcpostgres.WithUsername("llmgw"),
		tcpostgres.WithPassword("llmgw"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}
	return dsn
}

func TestTokenRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.LoadToken(ctx, "acct"); !errors.Is(err, domain.ErrTokenNotFound) {
		t.Fatalf("LoadToken(absent) error = %v, want domain.ErrTokenNotFound", err)
	}

	want := domain.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond),
	}
	if err := store.SaveToken(ctx, "acct", want); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	got, err := store.LoadToken(ctx, "acct")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	assertTokenEqual(t, got, want)

	rotated := domain.Token{
		AccessToken:  "access-2",
		RefreshToken: "refresh-2",
		ExpiresAt:    want.ExpiresAt.Add(8 * time.Hour),
	}
	if err := store.SaveToken(ctx, "acct", rotated); err != nil {
		t.Fatalf("SaveToken (rotate): %v", err)
	}

	got, err = store.LoadToken(ctx, "acct")
	if err != nil {
		t.Fatalf("LoadToken (after rotate): %v", err)
	}
	assertTokenEqual(t, got, rotated)
}

func TestWindowedTotals(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	projectID, err := store.EnsureProject(ctx, "metrics")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	now := time.Now().UTC()
	seedUsage(t, ctx, store, projectID, "news", now.Add(-30*time.Minute), 100, 40, 0.10)
	seedUsage(t, ctx, store, projectID, "news", now.Add(-2*time.Hour), 200, 80, 0.20)
	seedUsage(t, ctx, store, projectID, "feed", now.Add(-10*time.Minute), 50, 10, 0.05)
	seedUsage(t, ctx, store, projectID, "news", now.Add(-48*time.Hour), 999, 999, 9.99) // outside both windows

	hourAgo := now.Add(-time.Hour)
	dayAgo := now.Add(-24 * time.Hour)

	// Hourly window, news tag: only the 30-minute-old event.
	assertTotals(t, store, ctx, projectID, "news", hourAgo, domain.Totals{Calls: 1, InputTokens: 100, OutputTokens: 40, CostUSD: 0.10})

	// Daily window, news tag: the 30-minute + 2-hour events (the 48h one is excluded).
	assertTotals(t, store, ctx, projectID, "news", dayAgo, domain.Totals{Calls: 2, InputTokens: 300, OutputTokens: 120, CostUSD: 0.30})

	// Daily window, whole project: every tag inside 24h (news x2 + feed x1).
	assertTotals(t, store, ctx, projectID, domain.WholeProjectTag, dayAgo, domain.Totals{Calls: 3, InputTokens: 350, OutputTokens: 130, CostUSD: 0.35})

	// Hourly window, whole project: 30-minute news + 10-minute feed.
	assertTotals(t, store, ctx, projectID, domain.WholeProjectTag, hourAgo, domain.Totals{Calls: 2, InputTokens: 150, OutputTokens: 50, CostUSD: 0.15})
}

// seedUsage records one usage_event for the project/tag at the given timestamp.
func seedUsage(t *testing.T, ctx context.Context, store *Store, projectID int64, tag string, ts time.Time, in, out int, cost float64) {
	t.Helper()

	event := domain.UsageEvent{
		Timestamp:    ts,
		ProjectID:    projectID,
		Tag:          tag,
		Model:        "claude-sonnet-4-6",
		Provider:     DefaultProviderName,
		InputTokens:  in,
		OutputTokens: out,
		CostUSD:      cost,
		Status:       "ok",
		LatencyMS:    123,
	}
	if err := store.RecordUsage(ctx, event); err != nil {
		t.Fatalf("RecordUsage(%s @ %s): %v", tag, ts, err)
	}
}

// assertTotals fails unless WindowedTotals for the (project, tag, since) matches want.
func assertTotals(t *testing.T, store *Store, ctx context.Context, projectID int64, tag string, since time.Time, want domain.Totals) {
	t.Helper()

	got, err := store.WindowedTotals(ctx, projectID, tag, since)
	if err != nil {
		t.Fatalf("WindowedTotals(tag=%q): %v", tag, err)
	}
	if got.Calls != want.Calls || got.InputTokens != want.InputTokens || got.OutputTokens != want.OutputTokens {
		t.Errorf("WindowedTotals(tag=%q) counts = %+v, want %+v", tag, got, want)
	}
	if !floatsClose(got.CostUSD, want.CostUSD) {
		t.Errorf("WindowedTotals(tag=%q) cost = %v, want %v", tag, got.CostUSD, want.CostUSD)
	}
}

// floatsClose reports whether two USD amounts match within a cent's rounding tolerance.
func floatsClose(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}

// assertTokenEqual fails the test unless the two tokens carry identical credentials and expiry.
func assertTokenEqual(t *testing.T, got, want domain.Token) {
	t.Helper()

	if got.AccessToken != want.AccessToken {
		t.Errorf("access token = %q, want %q", got.AccessToken, want.AccessToken)
	}
	if got.RefreshToken != want.RefreshToken {
		t.Errorf("refresh token = %q, want %q", got.RefreshToken, want.RefreshToken)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("expires at = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
}
