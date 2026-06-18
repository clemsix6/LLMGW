package httpserver

import (
	"log"
	"net/http"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
)

// sendStreaming forwards a streaming request: it pre-sets the SSE response headers, relays the
// provider's SSE through a write-through sink (the first byte implicitly commits the 200 + the
// SSE headers), and records the usage_event from the accumulated usage. If the provider fails
// before any byte is written (e.g. an upstream non-200), nothing is committed yet, so the SSE
// headers are cleared and the error is mapped to an HTTP status exactly like the buffered path.
// A failure after relaying started leaves the 200 already sent, so it only stops the relay.
func (h *messagesHandler) sendStreaming(w http.ResponseWriter, r *http.Request, req llm.ChatRequest, projectID int64, tag string, provider domain.Provider) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming not supported by server")
		return
	}

	setSSEHeaders(w)
	sink := &streamingSink{writer: w, flusher: flusher}

	start := time.Now()
	metered, err := provider.Send(r.Context(), req, sink)
	latencyMS := time.Since(start).Milliseconds()

	status := statusOK
	errMsg := ""
	if err != nil {
		status, errMsg = statusError, err.Error()
	}
	h.record(r.Context(), projectID, tag, req.Model(), outcome{status, errMsg, metered, latencyMS})

	h.finishStream(w, sink, err, projectID, tag)
}

// finishStream maps a pre-stream failure to an HTTP error (the 200 was not committed yet) or
// logs a mid-stream abort (the 200 is already sent, so only the relay stops).
func (h *messagesHandler) finishStream(w http.ResponseWriter, sink *streamingSink, err error, projectID int64, tag string) {
	if err == nil {
		return
	}
	if !sink.wrote {
		clearSSEHeaders(w)
		writeProviderError(w, err)
		return
	}
	log.Printf("llmgw: streaming aborted after response started (project=%d tag=%q): %v", projectID, tag, err)
}

// streamingSink is the StreamSink for streaming responses: it writes through to the
// ResponseWriter (the first write implicitly commits 200 with the pre-set SSE headers) and
// flushes via http.Flusher so each relayed event reaches the consumer immediately.
type streamingSink struct {
	writer http.ResponseWriter // writer is the response the SSE stream is relayed into.

	flusher http.Flusher // flusher pushes each relayed event to the consumer.

	wrote bool // wrote reports whether any byte has been written (and thus 200 committed).
}

// Write relays bytes to the consumer, recording that the response has been committed.
func (s *streamingSink) Write(p []byte) (int, error) {
	s.wrote = true
	return s.writer.Write(p)
}

// Flush pushes the buffered event to the consumer.
func (s *streamingSink) Flush() {
	s.flusher.Flush()
}

// setSSEHeaders sets the Server-Sent Events response headers prior to the first write.
func setSSEHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
}

// clearSSEHeaders removes the SSE headers when a pre-stream failure means the response becomes a
// JSON error instead.
func clearSSEHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Del("Content-Type")
	header.Del("Cache-Control")
	header.Del("Connection")
}
