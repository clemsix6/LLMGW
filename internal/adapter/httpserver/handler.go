package httpserver

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// handler serves a single LLM gateway route: it resolves the project, enforces the budget,
// forwards to the injected provider, and records the usage_event. The provider and wire are
// resolved at construction so each request incurs no additional routing lookup.
type handler struct {
	store domain.Store // store resolves projects, limits, prices, and persists usage.

	provider domain.Provider // provider is the upstream backend for this route.

	w Wire // w parses request bodies and supplies the default tag for this route.

	providerName string // providerName labels the backend on every recorded usage_event.

	defaultProject string // defaultProject is attributed to requests that omit X-Project.
}

// newHandler constructs a handler with all dependencies injected.
func newHandler(store domain.Store, provider domain.Provider, w Wire, providerName, defaultProject string) *handler {
	return &handler{
		store:          store,
		provider:       provider,
		w:              w,
		providerName:   providerName,
		defaultProject: defaultProject,
	}
}

// handle validates the request envelope then forwards it (streaming and non-streaming alike).
func (h *handler) handle(w http.ResponseWriter, r *http.Request) {
	project, ok := resolveProject(r.Header.Get(headerProject), h.defaultProject)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_project", "X-Project header is required")
		return
	}
	tag := h.tagOrDefault(r.Header.Get(headerTags))

	req, ok := h.parseBody(w, r)
	if !ok {
		return
	}

	h.forward(w, r, req, project, tag)
}

// tagOrDefault returns the request tag from X-Tags, or the wire's default when absent.
func (h *handler) tagOrDefault(tag string) string {
	if tag == "" {
		return h.w.DefaultTag()
	}
	return tag
}

// parseBody reads and parses the request body through the route's wire, writing a 400 on failure.
func (h *handler) parseBody(w http.ResponseWriter, r *http.Request) (llm.Request, bool) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "read request body")
		return nil, false
	}

	req, err := h.w.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return nil, false
	}
	return req, true
}

// forward resolves the project, enforces the budget, then sends the request upstream.
// Budget enforcement (atomic pre-check + reservation) happens before forwarding so a blocked
// request never reaches the provider; a granted reservation is released after the call.
func (h *handler) forward(w http.ResponseWriter, r *http.Request, req llm.Request, project, tag string) {
	projectID, err := h.store.EnsureProject(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "resolve project")
		return
	}

	reservationID, ok := h.admit(w, r, req, project, projectID, tag)
	if !ok {
		return
	}
	if reservationID != 0 {
		defer h.release(reservationID)
	}

	h.send(w, r, req, projectID, tag)
}

// send dispatches to the streaming or buffered relay depending on the request mode.
func (h *handler) send(w http.ResponseWriter, r *http.Request, req llm.Request, projectID int64, tag string) {
	if req.Stream() {
		h.sendStreaming(w, r, req, projectID, tag)
		return
	}
	h.sendBuffered(w, r, req, projectID, tag)
}

// sendBuffered forwards a non-streaming request, records the usage_event, and relays the
// response. Usage is recorded before the success body is written so the row is committed by
// the time the consumer observes the 200. A provider error is recorded then mapped to HTTP.
func (h *handler) sendBuffered(w http.ResponseWriter, r *http.Request, req llm.Request, projectID int64, tag string) {
	sink := &bufferedSink{}
	start := time.Now()
	metered, err := h.provider.Send(r.Context(), req, sink)
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

// recordTimeout bounds the detached usage-recording writes so a wedged database cannot leak a
// goroutine after the client has gone.
const recordTimeout = 5 * time.Second

// record persists a usage_event. A recording failure is logged but never fails an otherwise
// successful proxy: the upstream quota was already spent, so retrying would double-charge.
// The usage_event is post-hoc accounting that must persist even after the client disconnects
// (which cancels the request context), so the writes run on a context detached from request
// cancellation with their own timeout — otherwise budget tracking silently drops every call
// whose client closes the connection the moment it has the full response.
func (h *handler) record(ctx context.Context, projectID int64, tag, model string, o outcome) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()

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

// costFor returns the notional USD cost of a metered call, or 0 when unpriced. A zero-token
// call skips the lookup entirely. A price-lookup error is logged and treated as unpriced so a
// recording failure never blocks the proxy.
func (h *handler) costFor(ctx context.Context, model string, u usage.Usage) float64 {
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
