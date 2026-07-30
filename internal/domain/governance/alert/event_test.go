package alert_test

import (
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

func TestKindTitleRendersTheTable(t *testing.T) {
	if title := alert.KindGatewayStarted.Title(); title != "Gateway started" {
		t.Fatalf("title = %q, want %q", title, "Gateway started")
	}
}

func TestKindTitleFallsBackToTheIdentifier(t *testing.T) {
	unknown := alert.Kind("something_nobody_mapped")

	if title := unknown.Title(); title != string(unknown) {
		t.Fatalf("title = %q, want %q", title, string(unknown))
	}
}
