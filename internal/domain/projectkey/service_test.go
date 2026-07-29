package projectkey

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

var errRepositoryFailure = errors.New("repository unavailable")

// TestNewServiceValidatesDependencies proves unsafe or incomplete service configuration is rejected.
func TestNewServiceValidatesDependencies(t *testing.T) {
	repo := newMemoryKeyRepository()
	pepper := bytes.Repeat([]byte("p"), 32)
	random := strings.NewReader(strings.Repeat("r", 44))
	now := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

	tests := []struct {
		name   string
		repo   governance.KeyRepository
		pepper []byte
		random io.Reader
		now    func() time.Time
	}{
		{name: "nil repository", pepper: pepper, random: random, now: now},
		{name: "short pepper", repo: repo, pepper: pepper[:31], random: random, now: now},
		{name: "nil random", repo: repo, pepper: pepper, now: now},
		{name: "nil clock", repo: repo, pepper: pepper, random: random},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewService(test.repo, test.pepper, test.random, test.now); err == nil {
				t.Fatal("NewService succeeded")
			}
		})
	}
}

// TestCreateNormalizesLabelsCopiesPepperAndAuthenticates proves the complete service happy path.
func TestCreateNormalizesLabelsCopiesPepperAndAuthenticates(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryKeyRepository()
	pepper := bytes.Repeat([]byte("p"), 32)
	originalPepper := append([]byte(nil), pepper...)
	now := time.Unix(1_700_000_000, 0).UTC()

	service, err := NewService(repo, pepper, strings.NewReader(strings.Repeat("r", 44)), func() time.Time {
		return now
	})
	if err != nil {
		t.Fatal(err)
	}
	pepper[0] = 'x'

	expiresAt := now.Add(time.Hour)
	created, err := service.Create(ctx, "  project alpha  ", "\tprimary key\t", &expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if created.Key.ProjectName != "project alpha" || created.Key.Name != "primary key" {
		t.Fatalf("created key labels = (%q, %q)", created.Key.ProjectName, created.Key.Name)
	}
	if created.Plaintext == "" || strings.Contains(created.Key.PublicID, created.Plaintext) {
		t.Fatalf("created key = %#v", created)
	}

	stored := repo.keys[created.Key.PublicID]
	wantDigest := Digest(originalPepper, created.Plaintext)
	if !bytes.Equal(stored.Digest, wantDigest[:]) {
		t.Fatalf("stored digest = %x, want %x", stored.Digest, wantDigest)
	}

	identity, err := service.Authenticate(ctx, created.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ProjectName != "project alpha" || identity.ClientKeyID != created.Key.ID {
		t.Fatalf("identity = %#v", identity)
	}
	if used := repo.keys[created.Key.PublicID].LastUsedAt; used == nil || !used.Equal(now) {
		t.Fatalf("last used = %v, want %v", used, now)
	}
}

// TestCreateRejectsUnsafeLabels proves operator-controlled labels are safe stable identifiers.
func TestCreateRejectsUnsafeLabels(t *testing.T) {
	service := newTestService(t, newMemoryKeyRepository(), strings.NewReader(strings.Repeat("r", 44)), fixedNow())
	invalidUTF8 := string([]byte{0xff})
	tooLong := strings.Repeat("x", 129)

	tests := []struct {
		name    string
		project string
		key     string
	}{
		{name: "empty project", project: " ", key: "key"},
		{name: "empty key", project: "project", key: "\t"},
		{name: "invalid UTF-8 project", project: invalidUTF8, key: "key"},
		{name: "invalid UTF-8 key", project: "project", key: invalidUTF8},
		{name: "control project", project: "bad\nproject", key: "key"},
		{name: "control key", project: "project", key: "bad\u007fkey"},
		{name: "long project", project: tooLong, key: "key"},
		{name: "long key", project: "project", key: tooLong},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.Create(context.Background(), test.project, test.key, nil); err == nil {
				t.Fatal("Create succeeded")
			}
		})
	}
}

// TestAuthenticateCollapsesCredentialFailures proves credential rejection has one public error.
func TestAuthenticateCollapsesCredentialFailures(t *testing.T) {
	now := fixedNow()()
	validToken, err := Generate(strings.NewReader(strings.Repeat("r", 44)))
	if err != nil {
		t.Fatal(err)
	}
	pepper := bytes.Repeat([]byte("p"), 32)
	validDigest := Digest(pepper, validToken.Plaintext)
	revokedAt := now.Add(-time.Minute)
	expiredAt := now

	tests := []struct {
		name string
		raw  string
		key  governance.ClientKey
	}{
		{name: "malformed", raw: "not-a-key"},
		{name: "unknown", raw: validToken.Plaintext},
		{name: "expired", raw: validToken.Plaintext, key: testClientKey(validToken.PublicID, validDigest[:], &expiredAt, nil)},
		{name: "revoked", raw: validToken.Plaintext, key: testClientKey(validToken.PublicID, validDigest[:], nil, &revokedAt)},
		{name: "digest mismatch", raw: validToken.Plaintext, key: testClientKey(validToken.PublicID, bytes.Repeat([]byte("x"), 32), nil, nil)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := newMemoryKeyRepository()
			if test.key.ID != 0 {
				repo.keys[test.key.PublicID] = test.key
			}
			service, err := NewService(repo, pepper, strings.NewReader(strings.Repeat("s", 44)), fixedNow())
			if err != nil {
				t.Fatal(err)
			}

			_, err = service.Authenticate(context.Background(), test.raw)
			if err != ErrInvalidCredential {
				t.Fatalf("Authenticate error = %v, want shared ErrInvalidCredential", err)
			}
		})
	}
}

// TestAuthenticatePreservesRepositoryFailures proves infrastructure failures remain distinguishable.
func TestAuthenticatePreservesRepositoryFailures(t *testing.T) {
	token, err := Generate(strings.NewReader(strings.Repeat("r", 44)))
	if err != nil {
		t.Fatal(err)
	}
	digest := Digest(bytes.Repeat([]byte("p"), 32), token.Plaintext)

	t.Run("lookup", func(t *testing.T) {
		repo := newMemoryKeyRepository()
		repo.lookupError = errRepositoryFailure
		service := newTestService(t, repo, strings.NewReader(strings.Repeat("s", 44)), fixedNow())
		assertInfrastructureError(t, service, token.Plaintext)
	})

	t.Run("last used update stays best effort", func(t *testing.T) {
		repo := newMemoryKeyRepository()
		repo.keys[token.PublicID] = testClientKey(token.PublicID, digest[:], nil, nil)
		repo.markError = errRepositoryFailure
		service := newTestService(t, repo, strings.NewReader(strings.Repeat("s", 44)), fixedNow())

		// Last-use tracking is observability: losing it must never reject a valid
		// credential, because that would turn a metadata write into an outage.
		identity, err := service.Authenticate(context.Background(), token.Plaintext)
		if err != nil {
			t.Fatalf("Authenticate error = %v, want a valid identity", err)
		}
		if identity.ClientKeyID != 1 || identity.ProjectID != 2 {
			t.Fatalf("identity = %#v, want the persisted key identity", identity)
		}
	})
}

// TestRotateRevokeAndList proves the remaining operator service operations map repository state.
func TestRotateRevokeAndList(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryKeyRepository()
	service := newTestService(t, repo, strings.NewReader(strings.Repeat("r", 88)), fixedNow())

	old, err := service.Create(ctx, "project", "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := service.Rotate(ctx, old.Key.ID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Plaintext == "" || replacement.Key.ID == old.Key.ID {
		t.Fatalf("replacement = %#v", replacement)
	}

	keys, err := service.List(ctx, " project ")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].ProjectName != "project" || keys[1].ProjectName != "project" {
		t.Fatalf("List(project) = %#v", keys)
	}
	if err := service.Revoke(ctx, replacement.Key.ID); err != nil {
		t.Fatal(err)
	}
	if repo.keys[replacement.Key.PublicID].RevokedAt == nil {
		t.Fatal("replacement was not revoked")
	}

	if _, err := service.List(ctx, "bad\nproject"); err == nil {
		t.Fatal("List accepted a control character")
	}
	if all, err := service.List(ctx, ""); err != nil || len(all) != 2 {
		t.Fatalf("List(all) = %#v, %v", all, err)
	}
}

// newTestService constructs a service with the standard test pepper.
func newTestService(t *testing.T, repo governance.KeyRepository, random io.Reader, now func() time.Time) *Service {
	t.Helper()

	service, err := NewService(repo, bytes.Repeat([]byte("p"), 32), random, now)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// fixedNow returns the stable clock used by service tests.
func fixedNow() func() time.Time {
	return func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
}

// testClientKey builds one valid persisted key fixture.
func testClientKey(publicID string, digest []byte, expiresAt, revokedAt *time.Time) governance.ClientKey {
	return governance.ClientKey{
		ID: 1, ProjectID: 2, ProjectName: "project", Name: "key", PublicID: publicID,
		Digest: append([]byte(nil), digest...), CreatedAt: fixedNow()(), ExpiresAt: expiresAt, RevokedAt: revokedAt,
	}
}

// assertInfrastructureError verifies Authenticate preserves a repository failure.
func assertInfrastructureError(t *testing.T, service *Service, raw string) {
	t.Helper()

	_, err := service.Authenticate(context.Background(), raw)
	if !errors.Is(err, errRepositoryFailure) || errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Authenticate error = %v, want repository failure", err)
	}
}

// memoryKeyRepository is a deterministic in-memory implementation of the key repository port.
type memoryKeyRepository struct {
	keys        map[string]governance.ClientKey // keys stores credentials by public identifier.
	order       []string                        // order preserves insertion order for list results.
	nextID      int64                           // nextID is assigned to the next inserted key.
	lookupError error                           // lookupError injects a lookup failure.
	markError   error                           // markError injects a last-used update failure.
}

// newMemoryKeyRepository creates an empty in-memory key repository.
func newMemoryKeyRepository() *memoryKeyRepository {
	return &memoryKeyRepository{keys: make(map[string]governance.ClientKey), nextID: 1}
}

// CreateKey inserts one project key.
func (r *memoryKeyRepository) CreateKey(_ context.Context, project, name, publicID string, digest []byte, expiresAt *time.Time) (governance.ClientKey, error) {
	key := governance.ClientKey{
		ID: r.nextID, ProjectID: 1, ProjectName: project, Name: name, PublicID: publicID,
		Digest: append([]byte(nil), digest...), CreatedAt: fixedNow()(), ExpiresAt: expiresAt,
	}
	r.nextID++
	r.keys[publicID] = key
	r.order = append(r.order, publicID)
	return key, nil
}

// KeyByPublicID returns one key or a zero value when absent.
func (r *memoryKeyRepository) KeyByPublicID(_ context.Context, publicID string) (governance.ClientKey, error) {
	if r.lookupError != nil {
		return governance.ClientKey{}, r.lookupError
	}
	return r.keys[publicID], nil
}

// RotateKey creates a replacement and applies the requested retirement policy to the old key.
func (r *memoryKeyRepository) RotateKey(_ context.Context, keyID int64, publicID string, digest []byte, now time.Time, overlap time.Duration) (governance.ClientKey, error) {
	oldID := r.publicIDForID(keyID)
	old := r.keys[oldID]
	if overlap <= 0 {
		old.RevokedAt = timePointer(now)
	} else {
		expiry := now.Add(overlap)
		if old.ExpiresAt != nil && old.ExpiresAt.Before(expiry) {
			expiry = *old.ExpiresAt
		}
		old.ExpiresAt = timePointer(expiry)
	}
	r.keys[oldID] = old
	return r.CreateKey(context.Background(), old.ProjectName, old.Name, publicID, digest, nil)
}

// ListKeys returns every key or the exact named project's keys.
func (r *memoryKeyRepository) ListKeys(_ context.Context, project string) ([]governance.ClientKey, error) {
	keys := make([]governance.ClientKey, 0, len(r.order))
	for _, publicID := range r.order {
		key := r.keys[publicID]
		if project == "" || key.ProjectName == project {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// MarkKeyUsed records a key's latest use.
func (r *memoryKeyRepository) MarkKeyUsed(_ context.Context, keyID int64, usedAt time.Time) error {
	if r.markError != nil {
		return r.markError
	}
	publicID := r.publicIDForID(keyID)
	key := r.keys[publicID]
	key.LastUsedAt = timePointer(usedAt)
	r.keys[publicID] = key
	return nil
}

// RevokeKey records a key's revocation.
func (r *memoryKeyRepository) RevokeKey(_ context.Context, keyID int64, revokedAt time.Time) error {
	publicID := r.publicIDForID(keyID)
	key := r.keys[publicID]
	key.RevokedAt = timePointer(revokedAt)
	r.keys[publicID] = key
	return nil
}

// ExpireKey records a key's expiry.
func (r *memoryKeyRepository) ExpireKey(_ context.Context, keyID int64, expiresAt time.Time) error {
	publicID := r.publicIDForID(keyID)
	key := r.keys[publicID]
	key.ExpiresAt = timePointer(expiresAt)
	r.keys[publicID] = key
	return nil
}

// publicIDForID resolves the public identifier for a database identifier.
func (r *memoryKeyRepository) publicIDForID(keyID int64) string {
	for publicID, key := range r.keys {
		if key.ID == keyID {
			return publicID
		}
	}
	return ""
}

// timePointer returns a pointer to an independent time value.
func timePointer(value time.Time) *time.Time {
	return &value
}
