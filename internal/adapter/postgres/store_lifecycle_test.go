package postgres

import (
	"context"
	"errors"
	"testing"
)

// TestStoreLifecycle catches a composition regression that returns a migrated store which cannot
// be pinged, or whose Close leaves its pool usable.
func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	dsn := startGovernancePostgres(t, ctx)
	store, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	store.Close()
	if err := store.Ping(ctx); err == nil {
		t.Fatal("Ping after Close succeeded")
	}
}

// TestServeSessionLockExcludesConcurrentStores catches replacing the dedicated
// session advisory lock with a transaction lock, a pooled one-shot query, or an
// early release that lets overlapping serve processes share one database.
func TestServeSessionLockExcludesConcurrentStores(t *testing.T) {
	ctx := context.Background()
	dsn := startGovernancePostgres(t, ctx)
	first, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	t.Cleanup(first.Close)
	second, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(second.Close)

	firstLock, err := first.AcquireServeLock(ctx)
	if err != nil {
		t.Fatalf("acquire first serve lock: %v", err)
	}
	t.Cleanup(func() { _ = firstLock.Release(context.Background()) })
	if err := first.Ping(ctx); err != nil {
		t.Fatalf("use first pool while lock held: %v", err)
	}

	if lock, err := second.AcquireServeLock(ctx); !errors.Is(err, ErrServeLockHeld) {
		if lock != nil {
			_ = lock.Release(context.Background())
		}
		t.Fatalf("concurrent serve lock = (%v, %v), want nil/ErrServeLockHeld", lock, err)
	}
	if err := firstLock.Release(ctx); err != nil {
		t.Fatalf("release first serve lock: %v", err)
	}

	secondLock, err := second.AcquireServeLock(ctx)
	if err != nil {
		t.Fatalf("acquire second serve lock after release: %v", err)
	}
	if err := secondLock.Release(ctx); err != nil {
		t.Fatalf("release second serve lock: %v", err)
	}
}
