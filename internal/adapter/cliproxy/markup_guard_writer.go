package cliproxy

import (
	"bufio"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/clemsix6/LLMGW/internal/domain/toolmarkup"
	"github.com/gin-gonic/gin"
)

// guardRejectionBody is the stable envelope a screened-out response is
// replaced with. A 502 is what makes the corruption retryable: clients treat
// it as a transient upstream failure instead of consuming a corrupted success.
const guardRejectionBody = `{"error":{"type":"upstream_protocol_error"}}`

// markupGuardWriter wraps the gin response writer of a flagged project's
// generation request and refuses to relay a non-streamed success whose
// tool_use inputs carry leaked tool-call markup.
//
// Streaming responses pass through untouched: their tool inputs arrive as
// partial JSON deltas no complete-document scan can screen, and a streaming
// client consumes its events long before finalization could veto them.
type markupGuardWriter struct {
	delegate  gin.ResponseWriter // delegate is the writer gin installed for this request.
	mode      writerMode         // mode is chosen at the first write and never re-decided.
	buffer    []byte             // buffer holds the whole body in buffered mode.
	overflow  bool               // overflow reports that buffered mode gave up screening.
	accepted  int                // accepted counts the body bytes taken from the handler.
	projectID int64              // projectID identifies the flagged project for the rejection log.
	requestID string             // requestID identifies the rejected request for the rejection log.
}

var _ gin.ResponseWriter = (*markupGuardWriter)(nil)

// newMarkupGuardWriter wraps one gin response writer.
func newMarkupGuardWriter(
	delegate gin.ResponseWriter,
	projectID int64,
	requestID string,
) *markupGuardWriter {
	return &markupGuardWriter{
		delegate:  delegate,
		projectID: projectID,
		requestID: requestID,
	}
}

// selectMode locks the wrapper's behaviour on the first byte written, for the
// same reasons toolPrefixWriter documents: the SDK's handlers set headers
// without calling WriteHeader on this wrapper, and a decision must never flip
// once bytes have moved.
func (w *markupGuardWriter) selectMode() {
	if w.mode != modeUndecided {
		return
	}
	switch {
	case w.delegate.Status() != http.StatusOK:
		// Error bodies carry no tool_use blocks and must reach the client verbatim.
		w.mode = modePassthrough
	case isEventStream(w.delegate.Header().Get("Content-Type")):
		w.mode = modePassthrough
		w.delegate.WriteHeaderNow()
	default:
		w.mode = modeBuffered
	}
}

// Write takes body bytes from the handler through the selected mode.
func (w *markupGuardWriter) Write(data []byte) (int, error) {
	w.selectMode()
	if w.mode == modeBuffered {
		return w.writeBuffered(data)
	}
	return w.delegate.Write(data)
}

// WriteString takes body bytes from the handler as a string, through the same
// modes as Write.
func (w *markupGuardWriter) WriteString(text string) (int, error) {
	w.selectMode()
	if w.mode == modeBuffered {
		return w.writeBuffered([]byte(text))
	}
	return w.delegate.WriteString(text)
}

// writeBuffered holds the body until finalization can screen it. Past
// maxRewriteBody the wrapper gives up screening, forwards what it holds, and
// lets every later write through unchanged.
func (w *markupGuardWriter) writeBuffered(data []byte) (int, error) {
	w.accepted += len(data)
	if w.overflow {
		return w.delegate.Write(data)
	}
	w.buffer = append(w.buffer, data...)
	if len(w.buffer) <= maxRewriteBody {
		return len(data), nil
	}
	w.overflow = true
	held := w.buffer
	w.buffer = nil
	if _, err := w.delegate.Write(held); err != nil {
		return 0, err
	}
	return len(data), nil
}

// WriteHeader records the response status while the wrapper has committed
// nothing, mirroring toolPrefixWriter.
func (w *markupGuardWriter) WriteHeader(code int) {
	if w.mode != modeUndecided {
		return
	}
	w.delegate.WriteHeader(code)
}

// WriteHeaderNow commits status and headers, except in buffered mode, where
// they are held until finalization has screened the body.
func (w *markupGuardWriter) WriteHeaderNow() {
	w.selectMode()
	if w.mode != modeBuffered {
		w.delegate.WriteHeaderNow()
	}
}

// Flush forwards to the delegate except in buffered mode, where committing
// would put the status on the wire before the screen can veto it.
func (w *markupGuardWriter) Flush() {
	w.selectMode()
	if w.mode == modeBuffered {
		return
	}
	w.delegate.Flush()
}

// finalize completes the wrapped response, and must run before the request is
// recorded complete: the recorded status is the one this decision produces.
func (w *markupGuardWriter) finalize() {
	if w.mode != modeBuffered || w.overflow {
		return
	}
	body := w.buffer
	w.buffer = nil

	snippet, found := toolmarkup.DetectResponse(body)
	if !found {
		w.forwardScreened(body)
		return
	}
	log.Printf(
		"llmgw: rejected leaked tool-call markup (project=%d request=%s): %s",
		w.projectID, w.requestID, snippet,
	)
	w.reject()
}

// forwardScreened writes the held body unchanged, with the length gin would
// otherwise have committed before the screen could run.
func (w *markupGuardWriter) forwardScreened(body []byte) {
	w.delegate.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.delegate.WriteHeaderNow()
	_, _ = w.delegate.Write(body)
}

// reject replaces the held success with the stable retryable envelope.
func (w *markupGuardWriter) reject() {
	w.delegate.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.delegate.Header().Set("Content-Length", strconv.Itoa(len(guardRejectionBody)))
	w.delegate.WriteHeader(http.StatusBadGateway)
	w.delegate.WriteHeaderNow()
	_, _ = w.delegate.WriteString(guardRejectionBody)
}

// Header exposes the delegate's header map. Buffered mode never commits it
// before finalization, so a handler's changes stay effective until then.
func (w *markupGuardWriter) Header() http.Header {
	return w.delegate.Header()
}

// Status returns the status the response carries.
func (w *markupGuardWriter) Status() int {
	return w.delegate.Status()
}

// Size returns the body bytes taken from the handler, which is the count the
// handler expects: past a rejection the two can no longer both be true.
func (w *markupGuardWriter) Size() int {
	if w.mode == modeBuffered {
		return w.accepted
	}
	return w.delegate.Size()
}

// Written reports whether the handler has produced any output, including bytes
// this wrapper still holds.
func (w *markupGuardWriter) Written() bool {
	return w.mode != modeUndecided || w.delegate.Written()
}

// Hijack surrenders the connection to the delegate.
func (w *markupGuardWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.delegate.Hijack()
}

// CloseNotify reports client disconnection from the delegate.
func (w *markupGuardWriter) CloseNotify() <-chan bool {
	return w.delegate.CloseNotify()
}

// Pusher exposes the delegate's server push, when it has one.
func (w *markupGuardWriter) Pusher() http.Pusher {
	return w.delegate.Pusher()
}
