package projectkey

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

const (
	minimumPepperBytes = 32
	maximumLabelBytes  = 128
)

// ErrInvalidCredential is returned for every rejected project credential.
var ErrInvalidCredential = errors.New("invalid credential")

// Service manages the secure project-key lifecycle.
type Service struct {
	repo   governance.KeyRepository // repo persists and resolves project keys.
	pepper []byte                   // pepper keys credential digests.
	random io.Reader                // random supplies key entropy.
	now    func() time.Time         // now returns the current authentication time.
}

// NewService validates dependencies and creates a project-key service.
func NewService(
	repo governance.KeyRepository,
	pepper []byte,
	random io.Reader,
	now func() time.Time,
) (*Service, error) {
	if repo == nil {
		return nil, errors.New("project key repository is required")
	}
	if len(pepper) < minimumPepperBytes {
		return nil, fmt.Errorf("project key pepper must contain at least %d bytes", minimumPepperBytes)
	}
	if random == nil {
		return nil, errors.New("project key random source is required")
	}
	if now == nil {
		return nil, errors.New("project key clock is required")
	}

	return &Service{
		repo:   repo,
		pepper: append([]byte(nil), pepper...),
		random: random,
		now:    now,
	}, nil
}

// Create validates operator labels and creates a project with its first or next key.
func (s *Service) Create(
	ctx context.Context,
	project string,
	name string,
	expiresAt *time.Time,
) (governance.CreatedKey, error) {
	project, err := normalizeLabel("project", project)
	if err != nil {
		return governance.CreatedKey{}, err
	}
	name, err = normalizeLabel("key", name)
	if err != nil {
		return governance.CreatedKey{}, err
	}

	token, err := Generate(s.random)
	if err != nil {
		return governance.CreatedKey{}, fmt.Errorf("generate project key:\n%w", err)
	}
	defer clear(token.Secret)

	digest := Digest(s.pepper, token.Plaintext)
	key, err := s.repo.CreateKey(ctx, project, name, token.PublicID, digest[:], expiresAt)
	if err != nil {
		return governance.CreatedKey{}, fmt.Errorf("create project key:\n%w", err)
	}
	return governance.CreatedKey{Key: keyInfo(key), Plaintext: token.Plaintext}, nil
}

// Authenticate validates a credential and records its successful use.
func (s *Service) Authenticate(ctx context.Context, raw string) (governance.KeyIdentity, error) {
	token, err := Parse(raw)
	if err != nil {
		return governance.KeyIdentity{}, ErrInvalidCredential
	}
	defer clear(token.Secret)

	key, err := s.repo.KeyByPublicID(ctx, token.PublicID)
	if err != nil {
		return governance.KeyIdentity{}, fmt.Errorf("look up project key:\n%w", err)
	}
	if key.ID == 0 {
		return governance.KeyIdentity{}, ErrInvalidCredential
	}

	digest := Digest(s.pepper, raw)
	now := s.now()
	if !hmac.Equal(key.Digest, digest[:]) || keyIsInactive(key, now) {
		return governance.KeyIdentity{}, ErrInvalidCredential
	}
	// Last-use tracking is observability, not authorization: it must never turn a
	// valid credential into a rejected request. Governance accounting is owned by
	// the request and usage tables, which are written on their own paths.
	_ = s.repo.MarkKeyUsed(ctx, key.ID, now)
	return keyIdentity(key), nil
}

// Rotate creates a replacement before retiring the old key under one transaction.
func (s *Service) Rotate(
	ctx context.Context,
	keyID int64,
	overlap time.Duration,
) (governance.CreatedKey, error) {
	token, err := Generate(s.random)
	if err != nil {
		return governance.CreatedKey{}, fmt.Errorf("generate replacement project key:\n%w", err)
	}
	defer clear(token.Secret)

	digest := Digest(s.pepper, token.Plaintext)
	key, err := s.repo.RotateKey(ctx, keyID, token.PublicID, digest[:], s.now(), overlap)
	if err != nil {
		return governance.CreatedKey{}, fmt.Errorf("rotate project key:\n%w", err)
	}
	return governance.CreatedKey{Key: keyInfo(key), Plaintext: token.Plaintext}, nil
}

// Revoke immediately revokes one key.
func (s *Service) Revoke(ctx context.Context, keyID int64) error {
	if err := s.repo.RevokeKey(ctx, keyID, s.now()); err != nil {
		return fmt.Errorf("revoke project key:\n%w", err)
	}
	return nil
}

// normalizeLabel trims and validates one operator-controlled label.
func normalizeLabel(kind string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximumLabelBytes || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid %s label", kind)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("invalid %s label", kind)
		}
	}
	return value, nil
}

// keyIsInactive reports whether a persisted key is revoked or expired at now.
func keyIsInactive(key governance.ClientKey, now time.Time) bool {
	if key.RevokedAt != nil {
		return true
	}
	return key.ExpiresAt != nil && !now.Before(*key.ExpiresAt)
}

// keyInfo removes secret digest material from persisted key metadata.
func keyInfo(key governance.ClientKey) governance.KeyInfo {
	return governance.KeyInfo{
		ID:          key.ID,
		ProjectID:   key.ProjectID,
		ProjectName: key.ProjectName,
		Name:        key.Name,
		PublicID:    key.PublicID,
		CreatedAt:   key.CreatedAt,
		ExpiresAt:   key.ExpiresAt,
		RevokedAt:   key.RevokedAt,
		LastUsedAt:  key.LastUsedAt,
	}
}

// keyIdentity maps a persisted key to its request identity.
func keyIdentity(key governance.ClientKey) governance.KeyIdentity {
	return governance.KeyIdentity{
		ProjectID:        key.ProjectID,
		ProjectName:      key.ProjectName,
		ClientKeyID:      key.ID,
		KeyName:          key.Name,
		PublicID:         key.PublicID,
		PrefixToolNames:  key.PrefixToolNames,
		DefaultEffort:    key.DefaultEffort,
		RejectToolMarkup: key.RejectToolMarkup,
	}
}
