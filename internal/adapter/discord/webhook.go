package discord

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// queueCapacity is how many events wait for delivery before drops start.
const queueCapacity = 256

// timings groups the delays so tests can shrink them.
type timings struct {
	backoffs      []time.Duration // backoffs is the wait before each retry, one entry per retry allowed.
	throttle      time.Duration   // throttle is the minimum gap between two deliveries.
	attempt       time.Duration   // attempt is the per-request timeout in steady state.
	drainAttempt  time.Duration   // drainAttempt is the per-request timeout while draining.
	maxRetryAfter time.Duration   // maxRetryAfter caps an honoured Retry-After.
}

// productionTimings is the schedule the gateway runs with.
func productionTimings() timings {
	return timings{
		backoffs:      []time.Duration{time.Second, 2 * time.Second},
		throttle:      time.Second,
		attempt:       10 * time.Second,
		drainAttempt:  3 * time.Second,
		maxRetryAfter: 30 * time.Second,
	}
}

// Webhook delivers alert events to one Discord webhook.
//
// Delivery happens on a single goroutine, so ordering is preserved and exactly
// one goroutine ever touches the socket. Notify only hands the event over.
type Webhook struct {
	url      string           // url is the Discord webhook endpoint.
	client   *http.Client     // client is the HTTP client the delivery goroutine owns.
	now      func() time.Time // now is the throttle clock.
	schedule timings          // schedule holds the retry, throttle and timeout delays.

	queue    chan alert.Event // queue buffers accepted events; it is never closed, so Notify can never panic.
	stopping chan struct{}    // stopping is closed by Close to stop the delivery goroutine.
	done     chan struct{}    // done is closed by the delivery goroutine when it returns.

	mu        sync.Mutex // mu guards accepting and dropped.
	accepting bool       // accepting reports whether Notify still enqueues.
	dropped   int        // dropped counts the events never handed to delivery.

	lastDelivery time.Time // lastDelivery is when the last delivery started; the delivery goroutine alone touches it.
}

// New builds a webhook with the production timings.
func New(webhookURL string, client *http.Client, now func() time.Time) *Webhook {
	return newWithTimings(webhookURL, client, now, productionTimings())
}

// newWithTimings builds a webhook with test-controlled delays.
//
// A nil client and a nil clock are both tolerated, so no call site has to build
// either just to construct the adapter.
func newWithTimings(webhookURL string, client *http.Client, now func() time.Time, schedule timings) *Webhook {
	if client == nil {
		client = &http.Client{Timeout: schedule.attempt}
	}
	if now == nil {
		now = time.Now
	}

	webhook := &Webhook{
		url:       webhookURL,
		client:    client,
		now:       now,
		schedule:  schedule,
		queue:     make(chan alert.Event, queueCapacity),
		stopping:  make(chan struct{}),
		done:      make(chan struct{}),
		accepting: true,
	}

	go webhook.consume()
	return webhook
}

// Notify hands one event to the delivery goroutine, reporting whether it was
// accepted.
//
// The tracker calls this with its own mutex held, so it never blocks: the send
// is non-blocking, the queue is never closed, and nothing is logged under the
// webhook's mutex either.
func (w *Webhook) Notify(event alert.Event) bool {
	if w == nil {
		return false
	}

	accepted, dropped := w.enqueue(event)
	if !accepted {
		log.Printf("llmgw: discord alert dropped (kind=%s, dropped=%d)", event.Kind, dropped)
	}
	return accepted
}

// enqueue performs the acceptance test and the non-blocking send under one
// mutex, which is what makes it atomic against Close flipping the flag.
func (w *Webhook) enqueue(event alert.Event) (bool, int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.accepting {
		select {
		case w.queue <- event:
			return true, 0
		default:
		}
	}

	w.dropped++
	return false, w.dropped
}

// consume delivers queued events one at a time until Close signals a stop.
//
// It returns without emptying the queue: Close owns what is left, because the
// drain delivers it newest first.
func (w *Webhook) consume() {
	defer close(w.done)

	for {
		// A plain two-case select picks randomly when both are ready, so the
		// stop signal is tested on its own first.
		if w.stopped() || !w.waitThrottle() {
			return
		}

		select {
		case <-w.stopping:
			return
		case event := <-w.queue:
			w.lastDelivery = w.now()
			w.deliverWithRetry(event)
		}
	}
}

// stopped reports whether Close has signalled.
func (w *Webhook) stopped() bool {
	select {
	case <-w.stopping:
		return true
	default:
		return false
	}
}

// waitThrottle waits out what is left of the gap since the last delivery,
// reporting false when Close signalled during the wait.
//
// It waits before taking an event rather than after delivering one, so a stop
// during the wait can never strand an event nothing will deliver.
func (w *Webhook) waitThrottle() bool {
	if w.lastDelivery.IsZero() {
		return true
	}
	return w.wait(w.schedule.throttle - w.now().Sub(w.lastDelivery))
}

// wait sleeps for delay, reporting false when Close signalled first. A
// non-positive delay never waits.
func (w *Webhook) wait(delay time.Duration) bool {
	if delay <= 0 {
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-w.stopping:
		return false
	case <-timer.C:
		return true
	}
}

// Close stops accepting events and delivers what is queued, newest first.
//
// It is safe to call twice and safe on a nil receiver. It reports an error only
// when ctx expired before the queue was emptied: a delivery that failed on its
// own is logged, so a caller's deferred Close stays quiet on the normal path.
func (w *Webhook) Close(ctx context.Context) error {
	if w == nil || !w.stopAccepting() {
		return nil
	}
	close(w.stopping)

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("close discord webhook:\n%w", err)
	}

	select {
	case <-w.done:
	case <-ctx.Done():
		// Returning here leaves the delivery goroutine to finish its in-flight
		// attempt alone, which is what keeps one goroutine on the socket.
		return fmt.Errorf("await discord delivery goroutine:\n%w", ctx.Err())
	}

	return w.drain(ctx)
}

// stopAccepting flips the accepting flag, reporting whether this call is the
// first: a second Close must not close the stop channel twice.
//
// The mutex is held for the flip alone — never across a wait, a sleep or an
// HTTP call, or Notify would block under the tracker's mutex for the whole
// shutdown budget.
func (w *Webhook) stopAccepting() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.accepting {
		return false
	}
	w.accepting = false
	return true
}

// drain delivers what the delivery goroutine left behind, newest first, one
// attempt each and no throttle.
//
// Newest-first is deliberate: gateway_stopping is enqueued last and is the
// event the drain exists for, so a FIFO drain under a short budget would
// deliver stale events and lose that one.
func (w *Webhook) drain(ctx context.Context) error {
	events := w.residual()

	for index := len(events) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("drain discord queue:\n%w", err)
		}
		w.deliverOnce(ctx, events[index])
	}
	return nil
}

// residual collects the events still queued. It is only ever called once the
// delivery goroutine has returned, so no one else is reading the queue.
func (w *Webhook) residual() []alert.Event {
	events := make([]alert.Event, 0, len(w.queue))

	for {
		select {
		case event := <-w.queue:
			events = append(events, event)
		default:
			return events
		}
	}
}
