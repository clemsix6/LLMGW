package discord

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// webhookToken stands in for the bearer secret a real webhook path carries.
const webhookToken = "TOKEN-that-must-never-reach-a-log"

// safeBuffer collects log output written by the delivery goroutine while the
// test reads it.
type safeBuffer struct {
	mu     sync.Mutex   // mu guards buffer against the delivery goroutine.
	buffer bytes.Buffer // buffer holds everything the logger wrote.
}

// Write appends one log record.
func (b *safeBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(payload)
}

// String returns everything logged so far.
func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

// captureLog redirects the standard logger for the duration of one test.
func captureLog(t *testing.T) *safeBuffer {
	t.Helper()

	collected := &safeBuffer{}
	flags := log.Flags()
	log.SetOutput(collected)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return collected
}

// TestTransportFailureKeepsTheWebhookTokenOutOfTheLog pins the redaction: a
// Discord webhook carries its token in the URL path, and a transport error
// prints the whole URL, so the drop log would otherwise publish the secret on
// exactly the failures it exists to report.
func TestTransportFailureKeepsTheWebhookTokenOutOfTheLog(t *testing.T) {
	refused := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := refused.URL
	refused.Close()

	logged := captureLog(t)
	webhook := newWithTimings(address+"/api/webhooks/123/"+webhookToken, nil, time.Now, fastTimings())
	defer func() { _ = webhook.Close(context.Background()) }()

	if !webhook.Notify(alert.Event{Kind: alert.KindGatewayStarted, Summary: "started"}) {
		t.Fatal("Notify() = false, want the event queued")
	}
	waitForDrop(t, logged)

	if strings.Contains(logged.String(), webhookToken) {
		t.Fatalf("drop log leaked the webhook token:\n%s", logged.String())
	}
}

// waitForDrop waits for the delivery goroutine to report its drop, which is
// also the only assertion covering logDrop itself.
func waitForDrop(t *testing.T, logged *safeBuffer) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logged.String(), "dropped after") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no drop reported within the deadline:\n%s", logged.String())
}
