package governance

import "time"

// Project identifies a governed project.
type Project struct {
	ID              int64     // ID is the database identifier.
	Name            string    // Name is the unique project name.
	CreatedAt       time.Time // CreatedAt is the UTC creation time.
	PrefixToolNames bool      // PrefixToolNames reports whether outbound tool names are namespaced for this project.
}

// ClientKey is a persisted project-scoped client credential.
type ClientKey struct {
	ID              int64      // ID is the database identifier.
	ProjectID       int64      // ProjectID identifies the owning project.
	ProjectName     string     // ProjectName is the owning project's unique name.
	Name            string     // Name is the operator-facing key name.
	PublicID        string     // PublicID is the non-secret lookup identifier.
	Digest          []byte     // Digest is the 32-byte digest of the key secret.
	CreatedAt       time.Time  // CreatedAt is the UTC creation time.
	ExpiresAt       *time.Time // ExpiresAt is the optional UTC expiry time.
	RevokedAt       *time.Time // RevokedAt is the optional UTC revocation time.
	LastUsedAt      *time.Time // LastUsedAt is the optional UTC last-use time.
	PrefixToolNames bool       // PrefixToolNames reports whether the owning project namespaces outbound tool names.
}

// KeyInfo is the non-secret representation of a client key.
type KeyInfo struct {
	ID          int64      // ID is the database identifier.
	ProjectID   int64      // ProjectID identifies the owning project.
	ProjectName string     // ProjectName is the owning project's unique name.
	Name        string     // Name is the operator-facing key name.
	PublicID    string     // PublicID is the non-secret lookup identifier.
	CreatedAt   time.Time  // CreatedAt is the UTC creation time.
	ExpiresAt   *time.Time // ExpiresAt is the optional UTC expiry time.
	RevokedAt   *time.Time // RevokedAt is the optional UTC revocation time.
	LastUsedAt  *time.Time // LastUsedAt is the optional UTC last-use time.
}

// KeyIdentity is the authenticated project identity carried by a request.
type KeyIdentity struct {
	ProjectID       int64  // ProjectID identifies the authenticated project.
	ProjectName     string // ProjectName is the authenticated project's unique name.
	ClientKeyID     int64  // ClientKeyID identifies the authenticated client key.
	KeyName         string // KeyName is the operator-facing key name.
	PublicID        string // PublicID is the key's non-secret lookup identifier.
	PrefixToolNames bool   // PrefixToolNames reports whether the authenticated project namespaces outbound tool names.
}

// CreatedKey combines persisted key metadata with its one-time plaintext value.
type CreatedKey struct {
	Key       KeyInfo // Key is the non-secret persisted key metadata.
	Plaintext string  // Plaintext is the key value returned only at creation.
}

// LegacyCredential is a credential imported from the legacy runtime.
type LegacyCredential struct {
	Provider         string     // Provider identifies the credential's provider.
	AccountLabel     string     // AccountLabel is the operator-facing account name.
	AccessToken      string     // AccessToken is the current provider access token.
	RefreshToken     string     // RefreshToken is the durable provider refresh token.
	ChatGPTAccountID string     // ChatGPTAccountID is the provider account identifier.
	ExpiresAt        *time.Time // ExpiresAt is the optional UTC access-token expiry.
}
