package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/projectkey"
)

// TestProjectKeyPersistence verifies the service and real governance schema keep only safe key data.
func TestProjectKeyPersistence(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	pepper := bytes.Repeat([]byte("p"), 32)
	random := bytes.NewReader(append(bytes.Repeat([]byte{1}, 44), bytes.Repeat([]byte{2}, 44)...))
	service := newProjectKeyService(t, store, pepper, random, func() time.Time { return now })

	initialProjects := countKeyProjects(t, ctx, store)
	unknown, err := projectkey.Generate(bytes.NewReader(bytes.Repeat([]byte{9}, 44)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, unknown.Plaintext); err != projectkey.ErrInvalidCredential {
		t.Fatalf("Authenticate(unknown) error = %v", err)
	}
	if got := countKeyProjects(t, ctx, store); got != initialProjects {
		t.Fatalf("projects after unknown authentication = %d, want %d", got, initialProjects)
	}

	created, err := service.Create(ctx, "  alpha  ", "\tprimary\t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.Key.ProjectName != "alpha" || created.Key.Name != "primary" {
		t.Fatalf("created labels = (%q, %q)", created.Key.ProjectName, created.Key.Name)
	}
	if got := countKeyProjects(t, ctx, store); got != initialProjects+1 {
		t.Fatalf("projects after Create = %d, want %d", got, initialProjects+1)
	}

	assertStoredCredential(t, ctx, store, pepper, created.Plaintext, created.Key.PublicID)
	assertNoSecretColumns(t, ctx, store)

	identity, err := service.Authenticate(ctx, created.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ProjectName != "alpha" || identity.ClientKeyID != created.Key.ID {
		t.Fatalf("identity = %#v", identity)
	}
	var lastUsedAt *time.Time
	if err := store.pool.QueryRow(ctx, `SELECT last_used_at FROM client_key WHERE id = $1`, created.Key.ID).Scan(&lastUsedAt); err != nil {
		t.Fatal(err)
	}
	if lastUsedAt == nil || !lastUsedAt.Equal(now) {
		t.Fatalf("last_used_at = %v, want %v", lastUsedAt, now)
	}

	second, err := service.Create(ctx, "beta", "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	all, err := service.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := service.List(ctx, " alpha ")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(alpha) != 1 || alpha[0].ID != created.Key.ID || second.Key.ProjectName != "beta" {
		t.Fatalf("List(all) = %#v; List(alpha) = %#v", all, alpha)
	}
	encoded, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(created.Plaintext)) {
		t.Fatalf("List exposed plaintext: %s", encoded)
	}
}

// TestKeyRotationImmediateAndOverlap verifies both retirement policies against real PostgreSQL.
func TestKeyRotationImmediateAndOverlap(t *testing.T) {
	t.Run("immediate", func(t *testing.T) {
		ctx := context.Background()
		store := newGovernanceStore(t)
		now := time.Unix(1_700_000_000, 0).UTC()
		random := joinedEntropy(1, 2)
		service := newProjectKeyService(t, store, bytes.Repeat([]byte("p"), 32), random, func() time.Time { return now })

		old, err := service.Create(ctx, "immediate", "primary", nil)
		if err != nil {
			t.Fatal(err)
		}
		replacement, err := service.Rotate(ctx, old.Key.ID, 0)
		if err != nil {
			t.Fatal(err)
		}

		storedOld, err := store.KeyByPublicID(ctx, old.Key.PublicID)
		if err != nil {
			t.Fatal(err)
		}
		if storedOld.RevokedAt == nil || !storedOld.RevokedAt.Equal(now) {
			t.Fatalf("old revoked_at = %v, want %v", storedOld.RevokedAt, now)
		}
		if replacement.Key.Name != old.Key.Name || replacement.Key.ProjectID != old.Key.ProjectID {
			t.Fatalf("replacement = %#v, old = %#v", replacement.Key, old.Key)
		}
		if _, err := service.Authenticate(ctx, old.Plaintext); err != projectkey.ErrInvalidCredential {
			t.Fatalf("Authenticate(old) error = %v", err)
		}
		if _, err := service.Authenticate(ctx, replacement.Plaintext); err != nil {
			t.Fatalf("Authenticate(replacement): %v", err)
		}
	})

	t.Run("overlap", func(t *testing.T) {
		ctx := context.Background()
		store := newGovernanceStore(t)
		current := time.Unix(1_700_000_000, 0).UTC()
		service := newProjectKeyService(
			t, store, bytes.Repeat([]byte("p"), 32), joinedEntropy(3, 4), func() time.Time { return current },
		)

		old, err := service.Create(ctx, "overlap", "primary", nil)
		if err != nil {
			t.Fatal(err)
		}
		replacement, err := service.Rotate(ctx, old.Key.ID, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}

		storedOld, err := store.KeyByPublicID(ctx, old.Key.PublicID)
		if err != nil {
			t.Fatal(err)
		}
		wantExpiry := current.Add(10 * time.Minute)
		if storedOld.RevokedAt != nil || storedOld.ExpiresAt == nil || !storedOld.ExpiresAt.Equal(wantExpiry) {
			t.Fatalf("overlap old = %#v, want expiry %v and no revocation", storedOld, wantExpiry)
		}
		if _, err := service.Authenticate(ctx, old.Plaintext); err != nil {
			t.Fatalf("Authenticate(overlapping old): %v", err)
		}
		if _, err := service.Authenticate(ctx, replacement.Plaintext); err != nil {
			t.Fatalf("Authenticate(overlapping replacement): %v", err)
		}

		current = wantExpiry
		if _, err := service.Authenticate(ctx, old.Plaintext); err != projectkey.ErrInvalidCredential {
			t.Fatalf("Authenticate(expired old) error = %v", err)
		}
	})

	t.Run("preserves earlier expiry", func(t *testing.T) {
		ctx := context.Background()
		store := newGovernanceStore(t)
		now := time.Unix(1_700_000_000, 0).UTC()
		service := newProjectKeyService(
			t, store, bytes.Repeat([]byte("p"), 32), joinedEntropy(5, 6), func() time.Time { return now },
		)
		earlier := now.Add(2 * time.Minute)
		old, err := service.Create(ctx, "earlier", "primary", &earlier)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Rotate(ctx, old.Key.ID, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		storedOld, err := store.KeyByPublicID(ctx, old.Key.PublicID)
		if err != nil {
			t.Fatal(err)
		}
		if storedOld.ExpiresAt == nil || !storedOld.ExpiresAt.Equal(earlier) {
			t.Fatalf("old expires_at = %v, want earlier %v", storedOld.ExpiresAt, earlier)
		}
	})
}

// TestKeyRotationRollsBackReplacementOnOldKeyUpdateFailure verifies replacement insert atomicity.
func TestKeyRotationRollsBackReplacementOnOldKeyUpdateFailure(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	service := newProjectKeyService(
		t, store, bytes.Repeat([]byte("p"), 32), joinedEntropy(7, 8), func() time.Time { return now },
	)

	old, err := service.Create(ctx, "rollback", "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	installKeyUpdateFailure(t, ctx, store)

	_, rotationErr := service.Rotate(ctx, old.Key.ID, 0)
	if rotationErr == nil {
		t.Fatal("Rotate succeeded despite injected old-row update failure")
	}
	if !strings.Contains(rotationErr.Error(), "injected failure after replacement insert") {
		t.Fatalf("Rotate error = %v, want proof replacement existed before old-row update", rotationErr)
	}

	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM client_key WHERE project_id = $1`, old.Key.ProjectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("client_key rows after failed rotation = %d, want 1", count)
	}

	var revokedAt, expiresAt *time.Time
	if err := store.pool.QueryRow(
		ctx, `SELECT revoked_at, expires_at FROM client_key WHERE id = $1`, old.Key.ID,
	).Scan(&revokedAt, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt != nil || expiresAt != nil {
		t.Fatalf("old key changed after rollback: revoked_at=%v expires_at=%v", revokedAt, expiresAt)
	}
}

// newProjectKeyService constructs a project key service for adapter tests.
func newProjectKeyService(
	t *testing.T,
	store *Store,
	pepper []byte,
	random *bytes.Reader,
	now func() time.Time,
) *projectkey.Service {
	t.Helper()

	service, err := projectkey.NewService(store, pepper, random, now)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// joinedEntropy returns distinct deterministic 44-byte chunks for consecutive generated keys.
func joinedEntropy(values ...byte) *bytes.Reader {
	var entropy []byte
	for _, value := range values {
		entropy = append(entropy, bytes.Repeat([]byte{value}, 44)...)
	}
	return bytes.NewReader(entropy)
}

// countKeyProjects counts projects without creating any.
func countKeyProjects(t *testing.T, ctx context.Context, store *Store) int {
	t.Helper()

	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM project`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// assertStoredCredential verifies the exact public identifier and keyed digest persisted.
func assertStoredCredential(
	t *testing.T,
	ctx context.Context,
	store *Store,
	pepper []byte,
	plaintext string,
	wantPublicID string,
) {
	t.Helper()

	var publicID string
	var digest []byte
	if err := store.pool.QueryRow(ctx, `SELECT public_id, digest FROM client_key WHERE public_id = $1`, wantPublicID).Scan(&publicID, &digest); err != nil {
		t.Fatal(err)
	}
	wantDigest := projectkey.Digest(pepper, plaintext)
	if publicID != wantPublicID || !bytes.Equal(digest, wantDigest[:]) || len(digest) != 32 {
		t.Fatalf("stored credential = (%q, %x), want (%q, %x)", publicID, digest, wantPublicID, wantDigest)
	}
	if bytes.Contains(digest, []byte(plaintext)) {
		t.Fatal("digest contains plaintext")
	}
}

// assertNoSecretColumns verifies the credential table has no plaintext or secret column.
func assertNoSecretColumns(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()

	var count int
	const query = `
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = 'client_key'
  AND column_name IN ('plaintext', 'secret')`
	if err := store.pool.QueryRow(ctx, query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("client_key secret-bearing columns = %d, want 0", count)
	}
}

// installKeyUpdateFailure aborts updates after proving a replacement row is already visible.
func installKeyUpdateFailure(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()

	const statement = `
CREATE FUNCTION fail_client_key_update() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    replacement_count integer;
BEGIN
    SELECT count(*) INTO replacement_count
    FROM client_key
    WHERE project_id = OLD.project_id AND id <> OLD.id;

    IF replacement_count <> 1 THEN
        RAISE EXCEPTION 'replacement missing before old-row update';
    END IF;

    RAISE EXCEPTION 'injected failure after replacement insert';
END;
$$;
CREATE TRIGGER fail_client_key_update
BEFORE UPDATE ON client_key
FOR EACH ROW EXECUTE FUNCTION fail_client_key_update();`
	if _, err := store.pool.Exec(ctx, statement); err != nil {
		t.Fatal(err)
	}
}

// TestProjectKeyRepositoryErrorsRemainWrapped verifies missing rows and database failures differ.
func TestProjectKeyRepositoryErrorsRemainWrapped(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)

	key, err := store.KeyByPublicID(ctx, "missing")
	if err != nil || key.ID != 0 {
		t.Fatalf("KeyByPublicID(missing) = %#v, %v", key, err)
	}

	store.pool.Close()
	if _, err := store.KeyByPublicID(ctx, "missing"); err == nil || errors.Is(err, projectkey.ErrInvalidCredential) {
		t.Fatalf("KeyByPublicID(closed) error = %v", err)
	}
}
