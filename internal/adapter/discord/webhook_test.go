package discord

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// settleDelay is how long a case waits before asserting that no further request
// arrived. It is far above the test backoffs and far below the test budget.
const settleDelay = 50 * time.Millisecond

// recordingDiscord is a stub Discord endpoint. Each case drives its answers
// through the respond function and reads what arrived through requests.
type recordingDiscord struct {
	server *httptest.Server // server is the endpoint the webhook posts to.

	mu     sync.Mutex // mu guards bodies.
	bodies [][]byte   // bodies holds every received request body, in arrival order.

	arrived chan struct{} // arrived carries one element per received request.
}

// startRecordingDiscord starts a stub endpoint whose respond function is given
// the one-based number of the request it is answering.
func startRecordingDiscord(t *testing.T, respond func(count int, writer http.ResponseWriter)) *recordingDiscord {
	t.Helper()

	stub := &recordingDiscord{arrived: make(chan struct{}, 512)}
	stub.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		respond(stub.record(body), writer)
	}))

	t.Cleanup(stub.server.Close)
	return stub
}

// record stores one request body and reports how many have arrived.
func (s *recordingDiscord) record(body []byte) int {
	s.mu.Lock()
	count := len(s.bodies) + 1
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()

	s.arrived <- struct{}{}
	return count
}

// requests returns a copy of the bodies received so far.
func (s *recordingDiscord) requests() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([][]byte(nil), s.bodies...)
}

// awaitRequests blocks until count requests have arrived, failing the case
// rather than hanging when they never do.
func (s *recordingDiscord) awaitRequests(t *testing.T, count int) {
	t.Helper()

	for range count {
		select {
		case <-s.arrived:
		case <-time.After(2 * time.Second):
			t.Fatalf("received %d requests, want %d", len(s.requests()), count)
		}
	}
}

// fastTimings is the production schedule scaled down: the same three attempts,
// with waits short enough for the whole package to stay well under its budget.
func fastTimings() timings {
	return timings{
		backoffs:      []time.Duration{100 * time.Microsecond, 200 * time.Microsecond},
		throttle:      100 * time.Microsecond,
		attempt:       2 * time.Second,
		drainAttempt:  2 * time.Second,
		maxRetryAfter: time.Millisecond,
	}
}

// webhookForTest builds a webhook closed when the case ends. It passes a nil
// client and a nil clock deliberately: the constructor must tolerate both.
func webhookForTest(t *testing.T, url string, schedule timings) *Webhook {
	t.Helper()

	webhook := newWithTimings(url, nil, nil, schedule)
	t.Cleanup(func() { _ = webhook.Close(expiredContext()) })

	return webhook
}

// expiredContext returns a context that is already past its deadline.
func expiredContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
}

// alwaysAnswer builds a respond function returning one fixed status.
func alwaysAnswer(status int) func(int, http.ResponseWriter) {
	return func(_ int, writer http.ResponseWriter) { writer.WriteHeader(status) }
}

func TestWebhookDeliversRenderedPayload(t *testing.T) {
	stub := startRecordingDiscord(t, alwaysAnswer(http.StatusNoContent))
	webhook := webhookForTest(t, stub.server.URL, fastTimings())

	event := alert.Event{
		Kind:     alert.KindGatewayStarted,
		Severity: alert.SeverityInfo,
		Fields:   []alert.Field{{Name: "Version", Value: "test"}},
		At:       renderInstant,
	}
	if !webhook.Notify(event) {
		t.Fatal("Notify = false, want true")
	}
	stub.awaitRequests(t, 1)

	want, err := renderPayload(event)
	if err != nil {
		t.Fatalf("renderPayload: %v", err)
	}

	requests := stub.requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if !bytes.Equal(requests[0], want) {
		t.Fatalf("body = %s, want %s", requests[0], want)
	}
}

func TestWebhookRetriesAfterRateLimit(t *testing.T) {
	stub := startRecordingDiscord(t, func(count int, writer http.ResponseWriter) {
		if count == 1 {
			writer.Header().Set("Retry-After", "0")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	webhook := webhookForTest(t, stub.server.URL, fastTimings())

	webhook.Notify(alert.Event{Kind: alert.KindGatewayStarted, At: renderInstant})
	stub.awaitRequests(t, 2)
	time.Sleep(settleDelay)

	if requests := stub.requests(); len(requests) != 2 {
		t.Fatalf("requests = %d, want 2 — a 429 costs one attempt and then succeeds", len(requests))
	}
}

func TestWebhookStopsAfterThreeServerErrors(t *testing.T) {
	stub := startRecordingDiscord(t, alwaysAnswer(http.StatusInternalServerError))
	webhook := webhookForTest(t, stub.server.URL, fastTimings())

	webhook.Notify(alert.Event{Kind: alert.KindGatewayStarted, At: renderInstant})
	stub.awaitRequests(t, 3)
	time.Sleep(settleDelay)

	if requests := stub.requests(); len(requests) != 3 {
		t.Fatalf("requests = %d, want 3 attempts then a drop", len(requests))
	}
}

func TestWebhookDoesNotRetryClientError(t *testing.T) {
	stub := startRecordingDiscord(t, alwaysAnswer(http.StatusBadRequest))
	webhook := webhookForTest(t, stub.server.URL, fastTimings())

	webhook.Notify(alert.Event{Kind: alert.KindGatewayStarted, At: renderInstant})
	stub.awaitRequests(t, 1)
	time.Sleep(settleDelay)

	if requests := stub.requests(); len(requests) != 1 {
		t.Fatalf("requests = %d, want 1 — a 400 is permanent", len(requests))
	}
}

func TestWebhookRefusesWhenQueueSaturated(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	stub := startRecordingDiscord(t, func(_ int, writer http.ResponseWriter) {
		<-release
		writer.WriteHeader(http.StatusNoContent)
	})
	webhook := webhookForTest(t, stub.server.URL, fastTimings())

	// The first event parks the delivery goroutine inside the blocked handler,
	// so nothing drains the queue while it is being saturated.
	webhook.Notify(alert.Event{Kind: alert.KindGatewayStarted, At: renderInstant})
	stub.awaitRequests(t, 1)

	started := time.Now()
	refused := saturate(webhook)

	if !refused {
		t.Fatal("Notify never returned false, want a refusal once the queue is full")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("saturating the queue took %s, want a non-blocking Notify", elapsed)
	}
}

// saturate notifies until the queue refuses, reporting whether it ever did.
func saturate(webhook *Webhook) bool {
	for range queueCapacity + 8 {
		if !webhook.Notify(alert.Event{Kind: alert.KindGatewayStarted, At: renderInstant}) {
			return true
		}
	}
	return false
}

func TestWebhookDrainsNewestFirst(t *testing.T) {
	stub := startRecordingDiscord(t, alwaysAnswer(http.StatusNoContent))

	// A throttle no test can outlive parks the consumer after the first
	// delivery, so the rest of the queue is still there for the drain.
	schedule := fastTimings()
	schedule.throttle = time.Hour
	webhook := webhookForTest(t, stub.server.URL, schedule)

	webhook.Notify(alert.Event{Kind: alert.KindGatewayStarted, At: renderInstant})
	stub.awaitRequests(t, 1)

	webhook.Notify(alert.Event{Kind: alert.KindBudgetWarning, At: renderInstant})
	webhook.Notify(alert.Event{Kind: alert.KindGatewayStopping, At: renderInstant})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := webhook.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	assertKinds(t, stub.requests(), []alert.Kind{
		alert.KindGatewayStarted,
		alert.KindGatewayStopping,
		alert.KindBudgetWarning,
	})
}

// assertKinds checks that the received bodies carry the wanted kinds in order,
// matching on each kind's rendered title.
func assertKinds(t *testing.T, requests [][]byte, kinds []alert.Kind) {
	t.Helper()

	if len(requests) != len(kinds) {
		t.Fatalf("requests = %d, want %d", len(requests), len(kinds))
	}

	for index, kind := range kinds {
		if !bytes.Contains(requests[index], []byte(kind.Title())) {
			t.Fatalf("request %d = %s, want the title of %q", index, requests[index], kind)
		}
	}
}

func TestWebhookSecondCloseIsQuiet(t *testing.T) {
	stub := startRecordingDiscord(t, alwaysAnswer(http.StatusNoContent))
	webhook := webhookForTest(t, stub.server.URL, fastTimings())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := webhook.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := webhook.Close(ctx); err != nil {
		t.Fatalf("second Close: %v, want nil", err)
	}
	if webhook.Notify(alert.Event{Kind: alert.KindGatewayStarted, At: renderInstant}) {
		t.Fatal("Notify = true after Close, want false")
	}
}

func TestRetryAfterParsesFractionalSeconds(t *testing.T) {
	cases := []struct {
		header string
		delay  time.Duration
		asked  bool
	}{
		{"0.75", 750 * time.Millisecond, true},
		{"0", 0, true},
		{"-1", 0, true},
		{"120", 30 * time.Second, true},
		{"", 0, false},
		{"soon", 0, false},
	}

	for _, testCase := range cases {
		delay, asked := retryAfter(testCase.header, 30*time.Second)

		if delay != testCase.delay || asked != testCase.asked {
			t.Fatalf("retryAfter(%q) = %s, %t, want %s, %t", testCase.header, delay, asked, testCase.delay, testCase.asked)
		}
	}
}
