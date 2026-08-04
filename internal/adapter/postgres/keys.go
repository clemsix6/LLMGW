package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// Confirm Store implements the project-key repository port.
var _ governance.KeyRepository = (*Store)(nil)

// Confirm Store implements the project-key expiry port.
var _ governance.KeyExpiryReader = (*Store)(nil)

// CreateKey creates its project if needed and inserts one project key atomically.
func (s *Store) CreateKey(
	ctx context.Context,
	project string,
	name string,
	publicID string,
	digest []byte,
	expiresAt *time.Time,
) (governance.ClientKey, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return governance.ClientKey{}, fmt.Errorf("begin project key creation:\n%w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	projectID, prefixToolNames, err := ensureKeyProject(ctx, tx, project)
	if err != nil {
		return governance.ClientKey{}, err
	}
	key, err := insertClientKey(ctx, tx, projectID, project, name, publicID, digest, expiresAt, prefixToolNames)
	if err != nil {
		return governance.ClientKey{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return governance.ClientKey{}, fmt.Errorf("commit project key creation:\n%w", err)
	}
	return key, nil
}

// KeyByPublicID resolves one project key without creating any project state.
func (s *Store) KeyByPublicID(ctx context.Context, publicID string) (governance.ClientKey, error) {
	const query = `
SELECT ck.id, ck.project_id, p.name, ck.name, ck.public_id, ck.digest,
       ck.created_at, ck.expires_at, ck.revoked_at, ck.last_used_at, p.prefix_tool_names
FROM client_key ck
JOIN project p ON p.id = ck.project_id
WHERE ck.public_id = $1`

	key, err := scanClientKey(s.pool.QueryRow(ctx, query, publicID))
	if errors.Is(err, pgx.ErrNoRows) {
		return governance.ClientKey{}, nil
	}
	if err != nil {
		return governance.ClientKey{}, fmt.Errorf("query project key by public identifier:\n%w", err)
	}
	return key, nil
}

// KeyByID resolves one client key by database identifier without exposing it in command output.
func (s *Store) KeyByID(ctx context.Context, keyID int64) (governance.ClientKey, error) {
	const query = `
SELECT ck.id, ck.project_id, p.name, ck.name, ck.public_id, ck.digest,
       ck.created_at, ck.expires_at, ck.revoked_at, ck.last_used_at, p.prefix_tool_names
FROM client_key ck
JOIN project p ON p.id = ck.project_id
WHERE ck.id = $1`
	key, err := scanClientKey(s.pool.QueryRow(ctx, query, keyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return governance.ClientKey{}, fmt.Errorf("find project key %d:\n%w", keyID, pgx.ErrNoRows)
	}
	if err != nil {
		return governance.ClientKey{}, fmt.Errorf("find project key %d:\n%w", keyID, err)
	}
	return key, nil
}

// RotateKey creates a replacement and retires the locked old key in one transaction.
func (s *Store) RotateKey(
	ctx context.Context,
	keyID int64,
	publicID string,
	digest []byte,
	now time.Time,
	overlap time.Duration,
) (governance.ClientKey, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return governance.ClientKey{}, fmt.Errorf("begin project key rotation:\n%w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	old, err := lockedClientKey(ctx, tx, keyID)
	if err != nil {
		return governance.ClientKey{}, err
	}
	replacement, err := insertClientKey(
		ctx, tx, old.ProjectID, old.ProjectName, old.Name, publicID, digest, nil, old.PrefixToolNames,
	)
	if err != nil {
		return governance.ClientKey{}, err
	}
	if err := retireClientKey(ctx, tx, keyID, now, overlap); err != nil {
		return governance.ClientKey{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return governance.ClientKey{}, fmt.Errorf("commit project key rotation:\n%w", err)
	}
	return replacement, nil
}

// ListKeys returns every key or only keys belonging to the exact named project.
func (s *Store) ListKeys(ctx context.Context, project string) ([]governance.ClientKey, error) {
	const query = `
SELECT ck.id, ck.project_id, p.name, ck.name, ck.public_id, ck.digest,
       ck.created_at, ck.expires_at, ck.revoked_at, ck.last_used_at, p.prefix_tool_names
FROM client_key ck
JOIN project p ON p.id = ck.project_id
WHERE ($1 = '' OR p.name = $1)
ORDER BY ck.id`

	rows, err := s.pool.Query(ctx, query, project)
	if err != nil {
		return nil, fmt.Errorf("query project keys:\n%w", err)
	}
	defer rows.Close()

	var keys []governance.ClientKey
	for rows.Next() {
		key, err := scanClientKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project key:\n%w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project keys:\n%w", err)
	}
	return keys, nil
}

// MarkKeyUsed records the key's most recent successful authentication.
func (s *Store) MarkKeyUsed(ctx context.Context, keyID int64, usedAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE client_key SET last_used_at = $2 WHERE id = $1`, keyID, usedAt); err != nil {
		return fmt.Errorf("update project key last use:\n%w", err)
	}
	return nil
}

// RevokeKey immediately revokes one project key.
func (s *Store) RevokeKey(ctx context.Context, keyID int64, revokedAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE client_key SET revoked_at = $2 WHERE id = $1`, keyID, revokedAt); err != nil {
		return fmt.Errorf("update project key revocation:\n%w", err)
	}
	return nil
}

// ExpiringKeys returns unrevoked keys expiring inside the inclusive window, earliest expiry first.
func (s *Store) ExpiringKeys(ctx context.Context, from, to time.Time) ([]governance.KeyInfo, error) {
	const query = `
SELECT ck.id, ck.project_id, p.name, ck.name, ck.public_id,
       ck.created_at, ck.expires_at, ck.revoked_at, ck.last_used_at
FROM client_key ck
JOIN project p ON p.id = ck.project_id
WHERE ck.revoked_at IS NULL
  AND ck.expires_at IS NOT NULL
  AND ck.expires_at BETWEEN $1 AND $2
ORDER BY ck.expires_at ASC`

	rows, err := s.pool.Query(ctx, query, from, to)
	if err != nil {
		return nil, fmt.Errorf("query expiring project keys:\n%w", err)
	}
	defer rows.Close()

	var keys []governance.KeyInfo
	for rows.Next() {
		key, err := scanKeyInfo(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expiring project key:\n%w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expiring project keys:\n%w", err)
	}
	return keys, nil
}

// ensureKeyProject returns the named project's identifier and current tool-name-prefix
// state, creating the project when absent.
func ensureKeyProject(ctx context.Context, tx pgx.Tx, project string) (int64, bool, error) {
	const query = `
INSERT INTO project (name) VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING id, prefix_tool_names`

	var projectID int64
	var prefixToolNames bool
	if err := tx.QueryRow(ctx, query, project).Scan(&projectID, &prefixToolNames); err != nil {
		return 0, false, fmt.Errorf("ensure key project:\n%w", err)
	}
	return projectID, prefixToolNames, nil
}

// insertClientKey inserts one key and returns its complete persisted representation.
// prefixToolNames carries the owning project's current state, resolved by the caller,
// since this insert never reads the project row itself.
func insertClientKey(
	ctx context.Context,
	tx pgx.Tx,
	projectID int64,
	project string,
	name string,
	publicID string,
	digest []byte,
	expiresAt *time.Time,
	prefixToolNames bool,
) (governance.ClientKey, error) {
	const query = `
INSERT INTO client_key (project_id, name, public_id, digest, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, created_at, expires_at, revoked_at, last_used_at`

	key := governance.ClientKey{
		ProjectID: projectID, ProjectName: project, Name: name, PublicID: publicID,
		Digest: append([]byte(nil), digest...), PrefixToolNames: prefixToolNames,
	}
	err := tx.QueryRow(ctx, query, projectID, name, publicID, digest, expiresAt).Scan(
		&key.ID, &key.CreatedAt, &key.ExpiresAt, &key.RevokedAt, &key.LastUsedAt,
	)
	if err != nil {
		return governance.ClientKey{}, fmt.Errorf("insert project key:\n%w", err)
	}
	return key, nil
}

// lockedClientKey loads and locks the old rotation row.
func lockedClientKey(ctx context.Context, tx pgx.Tx, keyID int64) (governance.ClientKey, error) {
	const query = `
SELECT ck.id, ck.project_id, p.name, ck.name, ck.public_id, ck.digest,
       ck.created_at, ck.expires_at, ck.revoked_at, ck.last_used_at, p.prefix_tool_names
FROM client_key ck
JOIN project p ON p.id = ck.project_id
WHERE ck.id = $1
FOR UPDATE OF ck`

	key, err := scanClientKey(tx.QueryRow(ctx, query, keyID))
	if err != nil {
		return governance.ClientKey{}, fmt.Errorf("lock project key for rotation:\n%w", err)
	}
	return key, nil
}

// retireClientKey revokes immediately or shortens expiry to the overlap boundary.
func retireClientKey(ctx context.Context, tx pgx.Tx, keyID int64, now time.Time, overlap time.Duration) error {
	var err error
	if overlap <= 0 {
		_, err = tx.Exec(ctx, `UPDATE client_key SET revoked_at = $2 WHERE id = $1`, keyID, now)
	} else {
		const query = `
UPDATE client_key
SET expires_at = CASE
    WHEN expires_at IS NULL OR expires_at > $2 THEN $2
    ELSE expires_at
END
WHERE id = $1`
		_, err = tx.Exec(ctx, query, keyID, now.Add(overlap))
	}
	if err != nil {
		return fmt.Errorf("retire old project key:\n%w", err)
	}
	return nil
}

// scanKeyInfo scans the joined client-key projection without the secret digest.
func scanKeyInfo(row pgx.Row) (governance.KeyInfo, error) {
	var key governance.KeyInfo
	err := row.Scan(
		&key.ID, &key.ProjectID, &key.ProjectName, &key.Name, &key.PublicID,
		&key.CreatedAt, &key.ExpiresAt, &key.RevokedAt, &key.LastUsedAt,
	)
	return key, err
}

// scanClientKey scans the common joined client-key projection. Shared by KeyByPublicID,
// KeyByID, ListKeys, and lockedClientKey: every one of those SELECTs must project the
// same columns in the same order, or the destination count mismatches at runtime.
func scanClientKey(row pgx.Row) (governance.ClientKey, error) {
	var key governance.ClientKey
	err := row.Scan(
		&key.ID, &key.ProjectID, &key.ProjectName, &key.Name, &key.PublicID, &key.Digest,
		&key.CreatedAt, &key.ExpiresAt, &key.RevokedAt, &key.LastUsedAt, &key.PrefixToolNames,
	)
	return key, err
}
