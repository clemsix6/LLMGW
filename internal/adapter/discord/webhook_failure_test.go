package discord

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// stalledTimings is fastTimings with per-request timeouts no case can outlive,
// so a parked handler keeps the attempt in flight until the case releases it
// rather than until the HTTP client gives up on its own.
func stalledTimings() timings {
	schedule := fastTimings()
	schedule.attempt = time.Minute
	schedule.drainAttempt = time.Minute

	return schedule
}

// failureEvent is the event the failure cases queue. Only the fact that it
// renders matters here: what they observe is the delivery, never the payload.
func failureEvent() alert.Event {
	return alert.Event{Kind: alert.KindGatewayStarted, At: renderInstant}
}

// passedDeadlineContext returns a context whose deadline is already behind it —
// the shutdown budget a Discord that stopped answering leaves a caller with.
func passedDeadlineContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	cancel()

	return ctx
}

// stalledWebhook returns a webhook whose first attempt is parked inside an
// unresponsive stub, the stub itself, and the release that lets that attempt
// answer at last.
//
// Release is idempotent and also registered as a cleanup, ahead of the stub's
// own: httptest.Server.Close blocks until every outstanding handler returns, so
// a handler parked forever would hang the package instead of failing one case.
func stalledWebhook(t *testing.T) (*Webhook, *recordingDiscord, func()) {
	t.Helper()

	release := make(chan struct{})
	closeOnce := sync.Once{}
	releaseHandler := func() { closeOnce.Do(func() { close(release) }) }

	stub := startRecordingDiscord(t, func(_ int, writer http.ResponseWriter) {
		<-release
		writer.WriteHeader(http.StatusNoContent)
	})
	t.Cleanup(releaseHandler)

	webhook := webhookForTest(t, stub.server.URL, stalledTimings())
	webhook.Notify(failureEvent())
	stub.awaitRequests(t, 1)

	return webhook, stub, releaseHandler
}

// assertAttemptInFlight fails when the delivery goroutine has already returned.
// Nothing but the release can make it return, so this is a fact rather than a
// race: the cases below all rest on the attempt still being stuck.
func assertAttemptInFlight(t *testing.T, webhook *Webhook) {
	t.Helper()

	select {
	case <-webhook.done:
		t.Fatal("the delivery goroutine returned, want it still stuck in its attempt")
	default:
	}
}

// assertOnlyStuckAttemptDelivered releases the stub, waits for the delivery
// goroutine, then checks that nothing beyond the stuck attempt reached the
// wire. Waiting on the goroutine rather than sleeping is what makes the count
// deterministic.
func assertOnlyStuckAttemptDelivered(t *testing.T, webhook *Webhook, stub *recordingDiscord, release func()) {
	t.Helper()

	release()
	select {
	case <-webhook.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the delivery goroutine never returned after the stub answered")
	}

	if requests := stub.requests(); len(requests) != 1 {
		t.Fatalf("requests = %d, want the stuck attempt alone", len(requests))
	}
}

func TestWebhookNotifyDoesNotBlockOnUnresponsiveServer(t *testing.T) {
	webhook, _, _ := stalledWebhook(t)

	accepted := make(chan bool, 1)
	go func() { accepted <- webhook.Notify(failureEvent()) }()

	select {
	case ok := <-accepted:
		if !ok {
			t.Fatal("Notify = false, want true — the queue is all but empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Notify never returned while an attempt was stuck in flight")
	}

	assertAttemptInFlight(t, webhook)
}

func TestWebhookCloseAbandonsStuckAttemptOnDeadline(t *testing.T) {
	webhook, stub, release := stalledWebhook(t)

	ctx, cancel := context.WithTimeout(context.Background(), settleDelay)
	defer cancel()

	started := time.Now()
	err := webhook.Close(ctx)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want a wrapped context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Close took %s, want it to give up on its own deadline", elapsed)
	}

	// Close returned on its budget, not on the attempt: the goroutine it left
	// behind is still holding the socket.
	assertAttemptInFlight(t, webhook)
	assertOnlyStuckAttemptDelivered(t, webhook, stub, release)
}

func TestWebhookCloseOnDoneContextDropsQueue(t *testing.T) {
	cases := []struct {
		name    string                 // name is the flavour of exhausted budget.
		context func() context.Context // context builds the already-done context Close is given.
		want    error                  // want is the sentinel the returned error must wrap.
	}{
		{name: "cancelled", context: expiredContext, want: context.Canceled},
		{name: "deadline passed", context: passedDeadlineContext, want: context.DeadlineExceeded},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			webhook, stub, release := stalledWebhook(t)

			// Two events pile up behind the stuck attempt. A Close with a budget
			// left would drain them; this one has none.
			webhook.Notify(failureEvent())
			webhook.Notify(failureEvent())

			started := time.Now()
			err := webhook.Close(testCase.context())

			if !errors.Is(err, testCase.want) {
				t.Fatalf("Close = %v, want a wrapped %v", err, testCase.want)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("Close took %s, want an immediate return on a done context", elapsed)
			}

			assertOnlyStuckAttemptDelivered(t, webhook, stub, release)
		})
	}
}

func TestWebhookNotifyAfterCutShortCloseIsRefused(t *testing.T) {
	webhook, stub, release := stalledWebhook(t)

	if err := webhook.Close(expiredContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want a wrapped context.Canceled", err)
	}

	// The delivery goroutine outlived that Close, which is the state a late
	// Notify has to survive: it is refused, and it never panics on a queue a
	// tidier shutdown might have closed.
	assertAttemptInFlight(t, webhook)

	for range 5 {
		if webhook.Notify(failureEvent()) {
			t.Fatal("Notify = true after Close, want false")
		}
	}

	assertOnlyStuckAttemptDelivered(t, webhook, stub, release)
}
