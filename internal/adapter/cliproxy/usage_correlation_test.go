package cliproxy

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestUsageBridgeRoundTripIsOpaqueAndAuthenticated(t *testing.T) {
	bridge := fixedUsageBridge(t)
	identity := RequestIdentity{
		RequestID:   "f5efc3a8-e6c3-49fd-bad6-6532fa51d216",
		ProjectID:   42,
		ClientKeyID: 7,
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
		Operation:   governance.OperationGeneration,
	}

	principal, ok := bridge.principal(identity)
	if !ok {
		t.Fatal("principal rejected a valid request identity")
	}
	if principal == identity.KeyPublicID || strings.Contains(principal, identity.RequestID) {
		t.Fatalf("principal exposes correlation fields: %q", principal)
	}
	correlation, ok := bridge.correlation(principal)
	if !ok {
		t.Fatal("correlation rejected an authentic principal")
	}
	if correlation.requestID != identity.RequestID ||
		correlation.keyPublicID != identity.KeyPublicID {
		t.Fatalf("correlation = %#v, want request and public key identity", correlation)
	}
}

func TestUsageBridgeRejectsMalformedAndTamperedPrincipals(t *testing.T) {
	bridge := fixedUsageBridge(t)
	principal, ok := bridge.principal(RequestIdentity{
		RequestID:   "f5efc3a8-e6c3-49fd-bad6-6532fa51d216",
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	})
	if !ok {
		t.Fatal("principal rejected valid fixture")
	}
	tampered := principal[:len(principal)-1] + differentPrincipalByte(principal[len(principal)-1])

	for _, candidate := range []string{"", "not-a-token", principal + ".extra", tampered} {
		if got, ok := bridge.correlation(candidate); ok || got != (usageCorrelation{}) {
			t.Fatalf("correlation(%q) = (%#v, %t), want zero, false", candidate, got, ok)
		}
	}
}

func TestNewUsageBridgeRequiresFullEntropy(t *testing.T) {
	if bridge, err := NewUsageBridge(bytes.NewReader(make([]byte, usageBridgeKeyBytes-1)), 1); err == nil || bridge != nil {
		t.Fatalf("NewUsageBridge(short entropy) = (%#v, %v), want nil, error", bridge, err)
	}
	for _, capacity := range []int{-1, 0} {
		if bridge, err := NewUsageBridge(bytes.NewReader(make([]byte, usageBridgeKeyBytes)), capacity); err == nil || bridge != nil {
			t.Fatalf("NewUsageBridge(capacity=%d) = (%#v, %v), want nil, error",
				capacity, bridge, err)
		}
	}
}

func TestUsageBridgeBoundsConcurrentGenerationGroups(t *testing.T) {
	const capacity = 8
	bridge := fixedUsageBridgeCapacity(t, capacity)
	identities := make([]RequestIdentity, capacity*4)
	for index := range identities {
		identities[index] = RequestIdentity{RequestID: deterministicRequestID(index)}
	}

	var group sync.WaitGroup
	var mu sync.Mutex
	admitted := make([]string, 0, capacity)
	for _, identity := range identities {
		group.Add(1)
		go func() {
			defer group.Done()
			if bridge.reserve(identity.RequestID) {
				mu.Lock()
				admitted = append(admitted, identity.RequestID)
				mu.Unlock()
			}
		}()
	}
	group.Wait()

	if len(admitted) != capacity || bridge.outstanding() != capacity {
		t.Fatalf("admitted/outstanding = %d/%d, want %d/%d",
			len(admitted), bridge.outstanding(), capacity, capacity)
	}
	if bridge.reserve(deterministicRequestID(len(identities) + 1)) {
		t.Fatal("reserve succeeded above capacity")
	}
	for _, requestID := range admitted {
		if !bridge.release(requestID) {
			t.Fatalf("release(%s) = false", requestID)
		}
	}
	if bridge.outstanding() != 0 ||
		!bridge.reserve(deterministicRequestID(len(identities)+2)) {
		t.Fatal("capacity did not return after release")
	}
}

func TestUsageBarrierIsPurposeSeparatedAuthenticatedAndSingleRelease(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   "f5efc3a8-e6c3-49fd-bad6-6532fa51d216",
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve rejected empty bridge")
	}
	var published sdkusage.Record
	bridge.publishRecord = func(_ context.Context, record sdkusage.Record) {
		published = record
	}

	bridge.publishBarrier(identity.RequestID)

	if published.APIKey == "" || !strings.HasPrefix(published.APIKey, usageBarrierPrefix+".") {
		t.Fatalf("barrier principal = %q", published.APIKey)
	}
	if _, ok := bridge.correlation(published.APIKey); ok {
		t.Fatal("usage principal validator accepted barrier purpose")
	}
	requestID, ok := bridge.barrierRequestID(published.APIKey)
	if !ok || requestID != identity.RequestID {
		t.Fatalf("barrier correlation = (%q, %t), want (%q, true)",
			requestID, ok, identity.RequestID)
	}
	if !bridge.release(requestID) || bridge.release(requestID) {
		t.Fatal("barrier did not release exactly once")
	}

	tampered := published.APIKey[:len(published.APIKey)-1] +
		differentPrincipalByte(published.APIKey[len(published.APIKey)-1])
	if requestID, ok := bridge.barrierRequestID(tampered); ok || requestID != "" {
		t.Fatalf("tampered barrier = (%q, %t), want empty, false", requestID, ok)
	}
}

func TestUsageBridgePoisonIsFailClosed(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 2)
	first := deterministicRequestID(1)
	if !bridge.reserve(first) {
		t.Fatal("reserve rejected empty bridge")
	}

	bridge.poison()

	if !bridge.poisoned() {
		t.Fatal("bridge is not poisoned")
	}
	if bridge.release(first) {
		t.Fatal("poisoned bridge released an outstanding permit")
	}
	if bridge.reserve(deterministicRequestID(2)) {
		t.Fatal("poisoned bridge admitted a new generation")
	}
}

func fixedUsageBridge(t *testing.T) *UsageBridge {
	return fixedUsageBridgeCapacity(t, 64)
}

func fixedUsageBridgeCapacity(t *testing.T, capacity int) *UsageBridge {
	t.Helper()
	bridge, err := NewUsageBridge(
		bytes.NewReader(bytes.Repeat([]byte{0x5a}, usageBridgeKeyBytes)),
		capacity,
	)
	if err != nil {
		t.Fatalf("NewUsageBridge: %v", err)
	}
	return bridge
}

func principalFor(t *testing.T, bridge *UsageBridge, identity RequestIdentity) string {
	t.Helper()
	principal, ok := bridge.principal(identity)
	if !ok {
		t.Fatalf("principal rejected identity %#v", identity)
	}
	return principal
}

func differentPrincipalByte(value byte) string {
	if value == 'A' {
		return "B"
	}
	return "A"
}

func deterministicRequestID(index int) string {
	return strings.ToLower(
		fmt.Sprintf("00000000-0000-4000-8000-%012x", index),
	)
}
