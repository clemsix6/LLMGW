package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestExpiringKeys verifies the window bounds, the revocation filter, and the
// ordering the alerting sweep depends on.
func TestExpiringKeys(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Now().UTC()

	seedExpiryKey(t, ctx, store, "expiry-soon", "pk-expiry-soon", ptrTime(now.Add(3*24*time.Hour)))
	seedExpiryKey(t, ctx, store, "expired-recently", "pk-expired-recently", ptrTime(now.Add(-2*24*time.Hour)))
	seedExpiryKey(t, ctx, store, "expired-long-ago", "pk-expired-long-ago", ptrTime(now.Add(-60*24*time.Hour)))
	seedExpiryKey(t, ctx, store, "never-expires", "pk-never-expires", nil)

	revoked := seedExpiryKey(t, ctx, store, "revoked-soon", "pk-revoked-soon", ptrTime(now.Add(24*time.Hour)))
	if err := store.RevokeKey(ctx, revoked.ID, now); err != nil {
		t.Fatalf("revoke project key: %v", err)
	}

	keys, err := store.ExpiringKeys(ctx, now.Add(-30*24*time.Hour), now.Add(7*24*time.Hour))
	if err != nil {
		t.Fatalf("ExpiringKeys: %v", err)
	}

	assertPublicIDs(t, keys, "pk-expired-recently", "pk-expiry-soon")
	assertKeyProjected(t, keys[0])
}

// assertPublicIDs fails unless the returned keys carry exactly these public
// identifiers in this order.
func assertPublicIDs(t *testing.T, keys []governance.KeyInfo, want ...string) {
	t.Helper()

	if len(keys) != len(want) {
		t.Fatalf("ExpiringKeys returned %d keys, want %d: %+v", len(keys), len(want), keys)
	}
	for i, publicID := range want {
		if keys[i].PublicID != publicID {
			t.Fatalf("key %d has public identifier %q, want %q", i, keys[i].PublicID, publicID)
		}
	}
}

// assertKeyProjected fails unless the projection carries the fields the
// alerting tracker reads beyond the expiry itself. A zero CreatedAt would make
// every key look long-lived and silently disable the short-lifetime skip.
func assertKeyProjected(t *testing.T, key governance.KeyInfo) {
	t.Helper()

	if key.CreatedAt.IsZero() {
		t.Fatalf("key %q has a zero creation time", key.PublicID)
	}
	if key.ProjectName != expiryProject {
		t.Fatalf("key %q has project name %q, want %q", key.PublicID, key.ProjectName, expiryProject)
	}
	if key.ExpiresAt == nil {
		t.Fatalf("key %q has no expiry", key.PublicID)
	}
	if key.RevokedAt != nil {
		t.Fatalf("key %q is revoked", key.PublicID)
	}
}

// expiryProject is the single project every expiry fixture key belongs to.
const expiryProject = "expiry-project"

// seedExpiryKey inserts one fixture key through the public creation path.
func seedExpiryKey(
	t *testing.T,
	ctx context.Context,
	store *Store,
	name string,
	publicID string,
	expiresAt *time.Time,
) governance.ClientKey {
	t.Helper()

	key, err := store.CreateKey(ctx, expiryProject, name, publicID, make([]byte, 32), expiresAt)
	if err != nil {
		t.Fatalf("create project key %q: %v", publicID, err)
	}
	return key
}

// ptrTime returns a pointer to its argument.
func ptrTime(at time.Time) *time.Time {
	return &at
}
