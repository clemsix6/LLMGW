package cliproxy

import (
	"context"
	"testing"
	"time"
)

// TestParkedCanceledGroupReturnsItsPermit catches the silent capacity leak: a
// canceled generation whose producer never publishes again — canceled before
// reaching an executor, or while the SDK waits out a cooldown — parks on its
// barrier forever. The permit is never returned, no poison ever fires because
// the group is not failed, and capacity melts request by request until every
// generation gets 503 behind a healthy status route.
func TestParkedCanceledGroupReturnsItsPermit(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	bridge.parkTimeout = 5 * time.Millisecond

	parked := "5f2b1c4a-0000-4000-8000-000000000011"
	if !bridge.reserve(parked) {
		t.Fatalf("reserve %s failed", parked)
	}
	bridge.persisted(parked, true)
	bridge.markCanceled(parked)
	if bridge.completeBarrier(parked, true) {
		t.Fatal("canceled barrier with no durable success completed instead of parking")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := bridge.waitDrained(ctx); err != nil {
		t.Fatalf("parked canceled group never returned its permit: %v", err)
	}

	next := "5f2b1c4a-0000-4000-8000-000000000012"
	if !bridge.reserve(next) {
		t.Fatal("permit still held after the parked group expired")
	}
	if bridge.poisoned() {
		t.Fatal("expiring a parked canceled group poisoned the bridge")
	}
}

// TestLateRecordAfterParkExpiryPersistsWithoutPoison verifies the tombstone: a
// canceled request's producer that publishes after the parked barrier expired
// is an expected straggler, so its authenticated record must persist normally
// instead of being read as a process-wide compatibility failure.
func TestLateRecordAfterParkExpiryPersistsWithoutPoison(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	bridge.parkTimeout = 5 * time.Millisecond

	expired := "5f2b1c4a-0000-4000-8000-000000000021"
	if !bridge.reserve(expired) {
		t.Fatalf("reserve %s failed", expired)
	}
	bridge.markCanceled(expired)
	bridge.completeBarrier(expired, true)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := bridge.waitDrained(ctx); err != nil {
		t.Fatalf("parked canceled group never expired: %v", err)
	}

	if !bridge.acceptRecord(expired) {
		t.Fatal("late record for an expired parked group was rejected")
	}
	if bridge.poisoned() {
		t.Fatal("late record for an expired parked group poisoned the bridge")
	}
	bridge.persisted(expired, false)

	stranger := "5f2b1c4a-0000-4000-8000-000000000022"
	if bridge.acceptRecord(stranger) {
		t.Fatal("record for a never-reserved group was accepted")
	}
	if !bridge.poisoned() {
		t.Fatal("record for a never-reserved group left the bridge unpoisoned")
	}
}

// TestPersistedRecordStillCompletesParkedGroupBeforeExpiry pins the existing
// contract: when the canceled stream's producer does publish within the parking
// window, that durable record completes the group at once, and a second record
// for the same completed group remains the compatibility failure it always was.
func TestPersistedRecordStillCompletesParkedGroupBeforeExpiry(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	bridge.parkTimeout = time.Hour

	parked := "5f2b1c4a-0000-4000-8000-000000000031"
	if !bridge.reserve(parked) {
		t.Fatalf("reserve %s failed", parked)
	}
	bridge.markCanceled(parked)
	bridge.completeBarrier(parked, true)
	bridge.persisted(parked, false)

	next := "5f2b1c4a-0000-4000-8000-000000000032"
	if !bridge.reserve(next) {
		t.Fatal("permit still held after a durable record completed the parked group")
	}
	if bridge.poisoned() {
		t.Fatal("completing a parked group through its durable record poisoned the bridge")
	}

	if bridge.acceptRecord(parked) {
		t.Fatal("second record for a completed group was accepted")
	}
	if !bridge.poisoned() {
		t.Fatal("second record for a completed group left the bridge unpoisoned")
	}
}
