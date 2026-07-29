package cliproxy

import (
	"bytes"
	"testing"
)

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
