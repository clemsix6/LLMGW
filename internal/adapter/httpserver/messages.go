package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

const (
	// headerProject carries the project name; it is required on every /v1/messages request.
	headerProject = "X-Project"

	// headerTags carries the budget bucket the call is attributed to.
	headerTags = "X-Tags"

	// defaultTag is the bucket used when X-Tags is absent.
	defaultTag = "default"

	// statusOK and statusError are the recorded outcomes of a usage_event.
	statusOK    = "ok"
	statusError = "error"
)

// messagesHandler serves POST /v1/messages: the Anthropic-compatible passthrough that resolves
// the project, forwards to the provider, and records usage. The handler owns HTTP status and
// headers; the provider only writes the response body into a buffered sink.
type messagesHandler struct {
	store domain.Store // store resolves projects, routes, and persists usage.

	providerName string // providerName labels the serving backend on each usage_event.
}

// handle validates the request envelope then forwards it (streaming and non-streaming alike).
func (h *messagesHandler) handle(w http.ResponseWriter, r *http.Request) {
	project := r.Header.Get(headerProject)
	if project == "" {
		writeError(w, http.StatusBadRequest, "missing_project", "X-Project header is required")
		return
	}
	tag := tagOrDefault(r.Header.Get(headerTags))

	req, ok := parseBody(w, r)
	if !ok {
		return
	}

	h.forward(w, r, req, project, tag)
}

// forward resolves the project and provider, enforces the budget, then sends the request
// upstream. Budget enforcement (atomic pre-check + reservation) happens before forwarding so a
// blocked request never reaches the provider; a granted reservation is released after the call.
func (h *messagesHandler) forward(w http.ResponseWriter, r *http.Request, req llm.ChatRequest, project, tag string) {
	projectID, err := h.store.EnsureProject(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "resolve project")
		return
	}

	provider, err := h.store.DefaultRoute(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "resolve provider")
		return
	}

	reservationID, ok := h.admit(w, r, req, project, projectID, tag)
	if !ok {
		return
	}
	if reservationID != 0 {
		defer h.release(reservationID)
	}

	h.send(w, r, req, projectID, tag, provider)
}

// send dispatches to the streaming or buffered relay depending on the request mode.
func (h *messagesHandler) send(w http.ResponseWriter, r *http.Request, req llm.ChatRequest, projectID int64, tag string, provider domain.Provider) {
	if req.Stream() {
		h.sendStreaming(w, r, req, projectID, tag, provider)
		return
	}
	h.sendBuffered(w, r, req, projectID, tag, provider)
}

// sendBuffered forwards a non-streaming request, records the usage_event, and relays the
// response. Usage is recorded before the success body is written so the row is committed by the
// time the consumer observes the 200. A provider error is recorded then mapped to an HTTP status.
func (h *messagesHandler) sendBuffered(w http.ResponseWriter, r *http.Request, req llm.ChatRequest, projectID int64, tag string, provider domain.Provider) {
	sink := &bufferedSink{}
	start := time.Now()
	metered, err := provider.Send(r.Context(), req, sink)
	latencyMS := time.Since(start).Milliseconds()

	if err != nil {
		h.record(r.Context(), projectID, tag, req.Model(), outcome{statusError, err.Error(), usage.Usage{}, latencyMS})
		writeProviderError(w, err)
		return
	}

	h.record(r.Context(), projectID, tag, req.Model(), outcome{statusOK, "", metered, latencyMS})
	writeSuccess(w, sink.buf.Bytes())
}

// outcome carries the result of a provider call for usage recording.
type outcome struct {
	status string // status is the recorded outcome ("ok" or "error").

	errMsg string // errMsg is the short error description, empty on success.

	usage usage.Usage // usage is the metered token counts.

	latencyMS int64 // latencyMS is the upstream call duration in milliseconds.
}

// record persists a usage_event. A recording failure is logged but never fails an otherwise
// successful proxy: the upstream quota was already spent, so retrying would double-charge.
func (h *messagesHandler) record(ctx context.Context, projectID int64, tag, model string, o outcome) {
	event := domain.UsageEvent{
		Timestamp:    time.Now().UTC(),
		ProjectID:    projectID,
		Tag:          tag,
		Model:        model,
		Provider:     h.providerName,
		InputTokens:  o.usage.InputTokens,
		OutputTokens: o.usage.OutputTokens,
		CostUSD:      h.costFor(ctx, model, o.usage),
		Status:       o.status,
		LatencyMS:    o.latencyMS,
		Error:        o.errMsg,
	}

	if err := h.store.RecordUsage(ctx, event); err != nil {
		log.Printf("llmgw: record usage (project=%d tag=%q): %v", projectID, tag, err)
	}
}

// costFor returns the notional USD cost of a metered call, or 0 when the model has no price row.
// An unpriced model records zero cost here (fail-closed budget enforcement is a later batch); a
// price-lookup failure is logged and treated as unpriced so a recording never blocks the proxy.
// A call that spent no tokens skips the lookup entirely.
func (h *messagesHandler) costFor(ctx context.Context, model string, u usage.Usage) float64 {
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return 0
	}

	in, out, ok, err := h.store.PriceFor(ctx, model)
	if err != nil {
		log.Printf("llmgw: price lookup (model=%q): %v", model, err)
		return 0
	}
	if !ok {
		return 0
	}
	return usage.Cost(u, in, out)
}

// parseBody reads and parses the Anthropic request body, writing a 400 on failure.
func parseBody(w http.ResponseWriter, r *http.Request) (llm.ChatRequest, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "read request body")
		return llm.ChatRequest{}, false
	}

	req, err := llm.ParseRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return llm.ChatRequest{}, false
	}
	return req, true
}

// tagOrDefault returns the request tag, falling back to defaultTag when the header is absent.
func tagOrDefault(tag string) string {
	if tag == "" {
		return defaultTag
	}
	return tag
}

// bufferedSink buffers what the provider relays so the handler can decide HTTP status and
// headers after Send returns. For non-streaming responses the provider writes the full body
// then flushes once; the handler copies the buffer to the consumer on success.
type bufferedSink struct {
	buf bytes.Buffer // buf accumulates the relayed response body.
}

// Write appends to the buffer.
func (s *bufferedSink) Write(p []byte) (int, error) {
	return s.buf.Write(p)
}

// Flush is a no-op: the buffer is read after Send returns.
func (s *bufferedSink) Flush() {}

// writeSuccess relays a buffered upstream response to the consumer as a 200.
func writeSuccess(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeProviderError maps a provider error to a clean HTTP status: all accounts cooling becomes
// 503 with a Retry-After, a rate limit 503 (with Retry-After when a reset is known), a dead
// refresh token 502, an upstream non-2xx its own status, and anything else 500.
func writeProviderError(w http.ResponseWriter, err error) {
	var cooling *claudemax.AllCoolingError
	if errors.As(err, &cooling) {
		w.Header().Set("Retry-After", retryAfterDuration(cooling.RetryAfter))
		writeError(w, http.StatusServiceUnavailable, "all_cooling", cooling.Error())
		return
	}

	var rate *claudemax.RateLimitError
	if errors.As(err, &rate) {
		if !rate.ResetAt.IsZero() {
			w.Header().Set("Retry-After", retryAfterSeconds(rate.ResetAt))
		}
		writeError(w, http.StatusServiceUnavailable, "rate_limited", rate.Error())
		return
	}

	var dead *claudemax.DeadRefreshTokenError
	if errors.As(err, &dead) {
		writeError(w, http.StatusBadGateway, "dead_refresh_token", dead.Error())
		return
	}

	var upstream *claudemax.UpstreamError
	if errors.As(err, &upstream) {
		writeError(w, upstreamStatus(upstream.Status), "upstream_error", upstream.Error())
		return
	}

	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

// upstreamStatus echoes a sane upstream HTTP status, falling back to 502 for nonsensical codes.
func upstreamStatus(status int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return http.StatusBadGateway
}

// retryAfterSeconds renders a Retry-After delay (whole seconds, at least one) until resetAt.
func retryAfterSeconds(resetAt time.Time) string {
	return retryAfterDuration(time.Until(resetAt))
}

// retryAfterDuration renders a Retry-After delay (whole seconds, at least one) for a duration.
func retryAfterDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}

// errorBody is the gateway's JSON error envelope for non-2xx responses.
type errorBody struct {
	Error errorDetail `json:"error"` // Error holds the typed error detail.
}

// errorDetail describes a gateway error: a stable machine-readable type and a human message.
type errorDetail struct {
	Type string `json:"type"` // Type is a stable error classifier (e.g. "rate_limited").

	Message string `json:"message"` // Message is a human-readable description.
}

// writeError writes a typed JSON error response with the given HTTP status.
func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Type: errType, Message: message}})
}
