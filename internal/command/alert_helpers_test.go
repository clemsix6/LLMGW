package command

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// testWebhookEnv names the environment variable every alerting fixture resolves
// the webhook URL from. It is named explicitly because applyLLMGWDefaults runs
// only through config.Load: a hand-built config.Config never receives the
// production default.
const testWebhookEnv = "TEST_DISCORD_WEBHOOK_URL"

// webhookStub is an httptest server standing in for Discord.
//
// Payloads are collected behind a mutex: the adapter delivers from its own
// goroutine, throttled in steady state and drained at shutdown, so a test never
// observes them synchronously.
type webhookStub struct {
	server *httptest.Server // server is the endpoint the fixtures point the webhook at.

	mu     sync.Mutex // mu guards bodies against the delivery goroutine.
	bodies []string   // bodies are the raw request payloads, in delivery order.
}

// newWebhookStub starts a stub webhook answering every delivery with 204.
func newWebhookStub(t *testing.T) *webhookStub {
	t.Helper()

	stub := &webhookStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			stub.record(string(body))
			writer.WriteHeader(http.StatusNoContent)
		},
	))
	t.Cleanup(stub.server.Close)
	return stub
}

// record stores one delivered payload.
func (s *webhookStub) record(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.bodies = append(s.bodies, body)
}

// received returns the payloads delivered so far.
func (s *webhookStub) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.bodies...)
}

// awaitAtLeast polls until count payloads arrived, reporting whether they did.
//
// Unlike waitFor it never touches *testing.T, so a fixture running on the
// gateway's own goroutine can synchronise on a delivery without failing the
// test from the wrong goroutine.
func (s *webhookStub) awaitAtLeast(count int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(s.received()) >= count {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// waitFor polls until at least count payloads arrived, under a bounded deadline.
func (s *webhookStub) waitFor(t *testing.T, count int) []string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if bodies := s.received(); len(bodies) >= count {
			return bodies
		}
		time.Sleep(5 * time.Millisecond)
	}

	bodies := s.received()
	t.Fatalf("deliveries = %d, want at least %d", len(bodies), count)
	return nil
}

// unreachableWebhookURL returns the URL of a server that already stopped
// listening, so every delivery attempt is refused rather than left hanging.
func unreachableWebhookURL(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	return server.URL
}

// deliveredPayload is the part of one rendered webhook body the composition
// tests assert on.
type deliveredPayload struct {
	Embeds []deliveredEmbed `json:"embeds"` // Embeds carries the single rendered event.
}

// deliveredEmbed is one rendered event as Discord receives it.
type deliveredEmbed struct {
	Title  string           `json:"title"`  // Title is the event kind's human title.
	Fields []deliveredField `json:"fields"` // Fields carry the identifying context.
}

// deliveredField is one labelled value of a rendered event.
type deliveredField struct {
	Name  string `json:"name"`  // Name labels the value.
	Value string `json:"value"` // Value is the rendered value.
}

// decodeDeliveries parses every collected body into its single embed.
func decodeDeliveries(t *testing.T, bodies []string) []deliveredEmbed {
	t.Helper()

	embeds := make([]deliveredEmbed, 0, len(bodies))
	for _, body := range bodies {
		var payload deliveredPayload
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("decode delivered payload %q: %v", body, err)
		}
		if len(payload.Embeds) != 1 {
			t.Fatalf("embeds = %d, want 1", len(payload.Embeds))
		}
		embeds = append(embeds, payload.Embeds[0])
	}
	return embeds
}

// embedFieldValue returns the value of the named rendered field, or the empty
// string when the embed does not carry it.
func embedFieldValue(embed deliveredEmbed, name string) string {
	for _, field := range embed.Fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

// alertFieldValue returns the value of the named field, or the empty string.
func alertFieldValue(fields []alert.Field, name string) string {
	for _, field := range fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

// runWithin fails the test unless action returns before the limit, so a
// delivery that hangs is reported rather than stalling the package.
func runWithin(t *testing.T, limit time.Duration, action func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		action()
	}()

	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("action did not return within %s", limit)
	}
}
