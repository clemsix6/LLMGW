package cliproxy

import (
	"context"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

func TestIdentityAbsent(t *testing.T) {
	if identity, ok := IdentityFromContext(context.Background()); ok {
		t.Fatalf("IdentityFromContext = %#v, true; want absent", identity)
	}
}

func TestIdentityStoredAsImmutableValue(t *testing.T) {
	identity := RequestIdentity{
		RequestID:   "request-1",
		ProjectID:   42,
		ClientKeyID: 7,
		KeyPublicID: "public-1",
		Operation:   governance.OperationGeneration,
	}
	ctx := WithIdentity(context.Background(), identity)

	identity.RequestID = "mutated"
	identity.ProjectID = 99

	got, ok := IdentityFromContext(ctx)
	if !ok {
		t.Fatal("IdentityFromContext reported no identity")
	}
	if got.RequestID != "request-1" || got.ProjectID != 42 {
		t.Fatalf("stored identity mutated: %#v", got)
	}

	got.RequestID = "second-mutation"
	again, _ := IdentityFromContext(ctx)
	if again.RequestID != "request-1" {
		t.Fatalf("context identity changed through returned value: %#v", again)
	}
}
