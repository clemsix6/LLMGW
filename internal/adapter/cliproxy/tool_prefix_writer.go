package cliproxy

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/clemsix6/LLMGW/internal/domain/toolprefix"
	"github.com/gin-gonic/gin"
)

const (
	// streamBufferLimit bounds how many bytes a streaming response may
	// accumulate without producing an event delimiter. A stream that never
	// delimits is one LLMGW should not be holding, so the buffer is forwarded
	// as-is and accumulation restarts.
	streamBufferLimit = 1 << 20
	// eventDelimiter terminates one server-sent event.
	eventDelimiter = "\n\n"
	// eventStreamContentType is the content type that selects streaming mode.
	eventStreamContentType = "text/event-stream"
)

// writerMode is the behaviour a toolPrefixWriter locks onto at its first write.
type writerMode uint8

const (
	// modeUndecided means nothing has been written yet.
	modeUndecided writerMode = iota
	// modePassthrough forwards every byte unchanged, whatever the content type.
	modePassthrough
	// modeStreaming rewrites complete events as the handler produces them.
	modeStreaming
	// modeBuffered holds the whole body and rewrites it at finalization.
	modeBuffered
)

// toolPrefixWriter wraps the gin response writer of a generation request and
// removes the outbound tool-name prefix on the way back to the client.
//
// Every gin.ResponseWriter method is implemented explicitly rather than
// promoted from an embedded delegate. WriteString and WriteHeaderNow are the
// two an embedded delegate would silently forward past the buffer, which
// reorders the body in buffered mode and interleaves unrewritten bytes into a
// stream.
type toolPrefixWriter struct {
	delegate gin.ResponseWriter // delegate is the writer gin installed for this request.
	mode     writerMode         // mode is chosen at the first write and never re-decided.
	buffer   []byte             // buffer holds a partial event, or the whole body.
	overflow bool               // overflow reports that buffered mode gave up rewriting.
	accepted int                // accepted counts the body bytes taken from the handler.
}

var _ gin.ResponseWriter = (*toolPrefixWriter)(nil)

// newToolPrefixWriter wraps one gin response writer.
func newToolPrefixWriter(delegate gin.ResponseWriter) *toolPrefixWriter {
	return &toolPrefixWriter{delegate: delegate}
}

// selectMode locks the wrapper's behaviour on the first byte written.
//
// The decision cannot be taken in WriteHeader: the SDK's Claude handlers set
// their content type with c.Header and then write, never calling WriteHeader
// on this wrapper, so a mode chosen there would never be chosen at all on
// exactly the responses the wrapper exists to rewrite. Once taken, the
// decision is final — the SDK reports a terminal streaming error by calling
// c.Status mid-stream, and re-deciding would flip a live stream into
// passthrough half-way through.
func (w *toolPrefixWriter) selectMode() {
	if w.mode != modeUndecided {
		return
	}
	switch {
	case w.delegate.Status() != http.StatusOK:
		// Error bodies carry no tool names and must reach the client verbatim.
		w.mode = modePassthrough
	case isEventStream(w.delegate.Header().Get("Content-Type")):
		w.mode = modeStreaming
		// A client waiting on a stream must not be held back by the wrapper.
		w.delegate.WriteHeaderNow()
	default:
		w.mode = modeBuffered
	}
}

// isEventStream reports whether a content type selects streaming mode.
func isEventStream(contentType string) bool {
	normalized := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(normalized, eventStreamContentType)
}

// Write takes body bytes from the handler through the selected mode.
func (w *toolPrefixWriter) Write(data []byte) (int, error) {
	w.selectMode()
	switch w.mode {
	case modeStreaming:
		return w.writeStream(data)
	case modeBuffered:
		return w.writeBuffered(data)
	default:
		return w.delegate.Write(data)
	}
}

// WriteString takes body bytes from the handler as a string, through the same
// modes as Write.
func (w *toolPrefixWriter) WriteString(text string) (int, error) {
	w.selectMode()
	switch w.mode {
	case modeStreaming:
		return w.writeStream([]byte(text))
	case modeBuffered:
		return w.writeBuffered([]byte(text))
	default:
		return w.delegate.WriteString(text)
	}
}

// writeStream accumulates event bytes and forwards every complete event with
// its tool names stripped, keeping any partial trailing event buffered.
func (w *toolPrefixWriter) writeStream(data []byte) (int, error) {
	w.accepted += len(data)
	w.buffer = append(w.buffer, data...)
	if err := w.drainEvents(); err != nil {
		return 0, err
	}
	return len(data), nil
}

// drainEvents forwards every complete event the buffer holds. A buffer that
// grows past streamBufferLimit without a delimiter is forwarded unchanged and
// accumulation restarts.
func (w *toolPrefixWriter) drainEvents() error {
	for {
		end := bytes.Index(w.buffer, []byte(eventDelimiter))
		if end < 0 {
			break
		}
		end += len(eventDelimiter)
		if _, err := w.delegate.Write(toolprefix.StripStreamEvent(w.buffer[:end])); err != nil {
			return err
		}
		w.buffer = w.buffer[end:]
	}
	if len(w.buffer) <= streamBufferLimit {
		return nil
	}
	return w.forwardHeld()
}

// writeBuffered holds the body until finalization can rewrite it. Past
// maxRewriteBody the wrapper gives up rewriting, forwards what it holds, and
// lets every later write through unchanged.
func (w *toolPrefixWriter) writeBuffered(data []byte) (int, error) {
	w.accepted += len(data)
	if w.overflow {
		return w.delegate.Write(data)
	}
	w.buffer = append(w.buffer, data...)
	if len(w.buffer) <= maxRewriteBody {
		return len(data), nil
	}
	w.overflow = true
	if err := w.forwardHeld(); err != nil {
		return 0, err
	}
	return len(data), nil
}

// forwardHeld writes whatever the buffer holds unchanged and empties it.
func (w *toolPrefixWriter) forwardHeld() error {
	held := w.buffer
	w.buffer = nil
	_, err := w.delegate.Write(held)
	return err
}

// WriteHeader records the response status while the wrapper has committed
// nothing. Once a mode is locked the status is fixed, mirroring gin, which
// refuses to change a status after the first write: the SDK calls c.Status
// mid-stream to report a terminal error, long after the client received its
// 200 and the streaming headers.
func (w *toolPrefixWriter) WriteHeader(code int) {
	if w.mode != modeUndecided {
		return
	}
	w.delegate.WriteHeader(code)
}

// WriteHeaderNow commits status and headers, except in buffered mode, where
// they are held until finalization can correct Content-Length.
func (w *toolPrefixWriter) WriteHeaderNow() {
	w.selectMode()
	if w.mode != modeBuffered {
		w.delegate.WriteHeaderNow()
	}
}

// Flush forwards to the delegate once every complete event has been passed on,
// so streaming latency is unchanged. Buffered mode holds the flush: committing
// there would put a stale Content-Length on the wire before the rewrite can
// correct it.
func (w *toolPrefixWriter) Flush() {
	w.selectMode()
	if w.mode == modeBuffered {
		return
	}
	if w.mode == modeStreaming {
		_ = w.drainEvents()
	}
	w.delegate.Flush()
}

// finalize completes the wrapped response, and must run before the request is
// recorded complete. In buffered mode it writes the rewritten body; in
// streaming mode it flushes residual bytes unchanged, since dropping them
// truncates the response into what reads as a protocol error.
func (w *toolPrefixWriter) finalize() {
	switch w.mode {
	case modeStreaming:
		if len(w.buffer) > 0 {
			_ = w.forwardHeld()
		}
	case modeBuffered:
		w.writeRewritten()
	}
}

// writeRewritten strips the tool-name prefix from the held body and writes it
// with the length the client actually receives — the rewrite shortens the
// body, so the length gin would have written before it is wrong.
func (w *toolPrefixWriter) writeRewritten() {
	if w.overflow {
		return
	}
	body := stripHeldBody(w.buffer)
	w.buffer = nil
	w.delegate.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.delegate.WriteHeaderNow()
	_, _ = w.delegate.Write(body)
}

// stripHeldBody rewrites the held JSON body, preserving any leading whitespace
// in front of it: a configured non-streaming keep-alive emits newline frames
// during a long generation, and the payload would read as malformed with them
// still attached.
func stripHeldBody(body []byte) []byte {
	document := bytes.TrimLeft(body, " \t\r\n")
	if len(document) == len(body) {
		return toolprefix.StripResponse(body)
	}

	keepAlive := body[:len(body)-len(document)]
	rewritten := toolprefix.StripResponse(document)
	restored := make([]byte, 0, len(keepAlive)+len(rewritten))
	restored = append(restored, keepAlive...)
	return append(restored, rewritten...)
}

// Header exposes the delegate's header map. Buffered mode never commits it
// before finalization, so a handler's changes stay effective until then.
func (w *toolPrefixWriter) Header() http.Header {
	return w.delegate.Header()
}

// Status returns the status the response carries.
func (w *toolPrefixWriter) Status() int {
	return w.delegate.Status()
}

// Size returns the body bytes taken from the handler, which is the count the
// handler expects: past the rewrite the two can no longer both be true.
func (w *toolPrefixWriter) Size() int {
	if w.mode == modeStreaming || w.mode == modeBuffered {
		return w.accepted
	}
	return w.delegate.Size()
}

// Written reports whether the handler has produced any output, including bytes
// this wrapper still holds. The SDK reads it before writing an error body, so
// a held response must never read as unwritten.
func (w *toolPrefixWriter) Written() bool {
	return w.mode != modeUndecided || w.delegate.Written()
}

// Hijack surrenders the connection to the delegate.
func (w *toolPrefixWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.delegate.Hijack()
}

// CloseNotify reports client disconnection from the delegate.
func (w *toolPrefixWriter) CloseNotify() <-chan bool {
	return w.delegate.CloseNotify()
}

// Pusher exposes the delegate's server push, when it has one.
func (w *toolPrefixWriter) Pusher() http.Pusher {
	return w.delegate.Pusher()
}
