package integration

import (
	"fmt"
	"os"
	"testing"
)

var testHarness *Harness

// TestMain owns the only SDK and default usage-manager lifecycle in this process.
func TestMain(m *testing.M) {
	harness, err := NewHarness()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration harness startup:\n%v\n", err)
		os.Exit(1)
	}
	testHarness = harness

	status := m.Run()
	if err := harness.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "integration harness shutdown:\n%v\n", err)
		status = 1
	}
	os.Exit(status)
}
