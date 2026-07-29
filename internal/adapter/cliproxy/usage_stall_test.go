package cliproxy

import "testing"

// TestExhaustedFailedPermitsPoisonInsteadOfFreezing catches the silent freeze:
// failed groups are never released, so once they fill every permit the bridge
// can neither admit nor drain, and without a terminal report the gateway
// refuses every generation while its health route still answers 200.
func TestExhaustedFailedPermitsPoisonInsteadOfFreezing(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 2)
	reported := 0
	bridge.ReportPoisonWith(func() { reported++ })

	first := "5f2b1c4a-0000-4000-8000-000000000001"
	second := "5f2b1c4a-0000-4000-8000-000000000002"
	for _, id := range []string{first, second} {
		if !bridge.reserve(id) {
			t.Fatalf("reserve %s failed", id)
		}
	}

	bridge.fail(first)
	if bridge.poisoned() {
		t.Fatal("bridge poisoned while a healthy permit could still drain")
	}

	bridge.fail(second)
	if !bridge.poisoned() {
		t.Fatal("every permit held by a failed group left the bridge unpoisoned")
	}
	if reported != 1 {
		t.Fatalf("terminal reports = %d, want exactly 1", reported)
	}
}
