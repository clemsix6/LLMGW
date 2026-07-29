package cliproxy

import (
	"context"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// RequestIdentity is the immutable project identity attached to an admitted request.
type RequestIdentity struct {
	RequestID   string               // RequestID is the governed request UUID.
	ProjectID   int64                // ProjectID identifies the authenticated project.
	ClientKeyID int64                // ClientKeyID identifies the authenticating key.
	KeyPublicID string               // KeyPublicID is the key's non-secret public identifier.
	Operation   governance.Operation // Operation identifies generation or metadata work.
}

// identityContextKey is private so only this package can attach request identities.
type identityContextKey struct{}

// WithIdentity returns a child context containing an immutable identity value.
func WithIdentity(ctx context.Context, identity RequestIdentity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFromContext returns the request identity attached by LLMGW middleware.
func IdentityFromContext(ctx context.Context) (RequestIdentity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(RequestIdentity)
	return identity, ok
}
