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
