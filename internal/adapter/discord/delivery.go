package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// attemptOutcome is what one attempt says about the next one.
type attemptOutcome struct {
	retry bool          // retry reports whether another attempt is worth making.
	delay time.Duration // delay is the wait a Retry-After asked for.
	asked bool          // asked reports whether the response carried a usable Retry-After.
	err   error         // err describes the failure; a nil err marks a delivered event.
}

// deliverWithRetry posts one event under the steady-state policy: one attempt
// per backoff plus the first, and a log line when the last one still failed.
func (w *Webhook) deliverWithRetry(event alert.Event) {
	body, rendered := payloadOf(event)
	if !rendered {
		return
	}

	for attempt := 0; ; attempt++ {
		outcome := w.post(context.Background(), body, w.schedule.attempt)
		if outcome.err == nil {
			return
		}

		if !outcome.retry || attempt == len(w.schedule.backoffs) {
			logDrop(event, attempt+1, outcome.err)
			return
		}
		if !w.wait(w.retryDelay(outcome, attempt)) {
			logDrop(event, attempt+1, outcome.err)
			return
		}
	}
}

// deliverOnce posts one event with a single attempt, under the caller's
// shutdown budget: the steady-state schedule cannot fit in it.
func (w *Webhook) deliverOnce(ctx context.Context, event alert.Event) {
	body, rendered := payloadOf(event)
	if !rendered {
		return
	}

	if outcome := w.post(ctx, body, w.schedule.drainAttempt); outcome.err != nil {
		logDrop(event, 1, outcome.err)
	}
}

// retryDelay is how long to wait before the attempt following this outcome: the
// Retry-After a 429 gave, the schedule's backoff otherwise.
func (w *Webhook) retryDelay(outcome attemptOutcome, attempt int) time.Duration {
	if outcome.asked {
		return outcome.delay
	}
	return w.schedule.backoffs[attempt]
}

// post performs one attempt under its own timeout, so a hung Discord can never
// hold the delivery goroutine longer than the schedule allows.
func (w *Webhook) post(parent context.Context, body []byte, timeout time.Duration) attemptOutcome {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return attemptOutcome{err: fmt.Errorf("build discord request:\n%w", err)}
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := w.client.Do(request)
	if err != nil {
		return attemptOutcome{retry: true, err: fmt.Errorf("post discord webhook:\n%w", err)}
	}
	defer discardBody(response)

	return classify(response, w.schedule.maxRetryAfter)
}

// classify turns one response into the outcome the retry policy reads: a 5xx
// and a 429 are worth another attempt, every other 4xx is permanent.
func classify(response *http.Response, maxRetryAfter time.Duration) attemptOutcome {
	switch {
	case response.StatusCode < 300:
		return attemptOutcome{}

	case response.StatusCode == http.StatusTooManyRequests:
		delay, asked := retryAfter(response.Header.Get("Retry-After"), maxRetryAfter)
		return attemptOutcome{
			retry: true,
			delay: delay,
			asked: asked,
			err:   errors.New("discord rate limited the webhook (HTTP 429)"),
		}

	case response.StatusCode >= 500:
		return attemptOutcome{retry: true, err: fmt.Errorf("discord answered HTTP %d", response.StatusCode)}

	default:
		return attemptOutcome{err: fmt.Errorf("discord rejected the payload (HTTP %d)", response.StatusCode)}
	}
}

// retryAfter reads a Retry-After header as a number of seconds, reporting false
// when it carried nothing usable and the normal backoff applies instead.
//
// It is parsed as a float because Discord sends fractional values such as
// "0.75", which an integer parser would reject in production while passing a
// test that only ever sends "0".
func retryAfter(header string, maxRetryAfter time.Duration) (time.Duration, bool) {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(header), 64)
	if err != nil {
		return 0, false
	}
	if seconds <= 0 {
		return 0, true
	}

	return min(time.Duration(seconds*float64(time.Second)), maxRetryAfter), true
}

// payloadOf renders one event, reporting false when it cannot be marshalled at
// all — such an event is dropped rather than retried.
func payloadOf(event alert.Event) ([]byte, bool) {
	body, err := renderPayload(event)
	if err != nil {
		log.Printf("llmgw: render discord alert (kind=%s): %v", event.Kind, err)
		return nil, false
	}
	return body, true
}

// discardBody drains and closes a response body, so the connection is reused
// and no server waits on an unread request.
func discardBody(response *http.Response) {
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}

// logDrop reports an event nobody will deliver. The log is the observation
// point: this repository has no metrics surface.
func logDrop(event alert.Event, attempts int, err error) {
	log.Printf("llmgw: deliver discord alert (kind=%s): dropped after %d attempt(s): %v", event.Kind, attempts, err)
}
