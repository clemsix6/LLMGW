package cliproxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/clemsix6/LLMGW/internal/domain/toolprefix"
	"github.com/gin-gonic/gin"
)

// maxRewriteBody bounds the payload LLMGW is willing to hold in memory for one
// tool-name rewrite, in either direction. It sits above Anthropic's own
// practical request ceiling, so only a request that would fail upstream anyway
// ever reaches it.
const maxRewriteBody = 32 << 20

// rewriteRequestBody replaces the request body with its tool-name-prefixed
// form, so the SDK handler reads the namespaced payload instead of the one the
// client sent. It reports false when the request was aborted, in which case
// the caller still owns whatever it reserved before calling.
func rewriteRequestBody(c *gin.Context) bool {
	if c.Request.Body == nil {
		return true
	}
	payload, ok := readBoundedBody(c)
	if !ok {
		return false
	}
	installRequestBody(c.Request, toolprefix.PrefixRequest(payload))
	return true
}

// readBoundedBody reads the whole request body under the maxRewriteBody
// ceiling. A declared length above the ceiling is refused before a single byte
// is buffered; an undeclared one is caught by the bounded reader instead.
func readBoundedBody(c *gin.Context) ([]byte, bool) {
	if c.Request.ContentLength > maxRewriteBody {
		abortSafe(c, http.StatusRequestEntityTooLarge, "request_too_large")
		return nil, false
	}

	bounded := http.MaxBytesReader(c.Writer, c.Request.Body, maxRewriteBody)
	payload, err := io.ReadAll(bounded)
	if err == nil {
		return payload, true
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		abortSafe(c, http.StatusRequestEntityTooLarge, "request_too_large")
		return nil, false
	}
	// A body the client stopped sending is the client's failure, not the
	// gateway's: it is refused without observing database health, which would
	// otherwise report an outage every time a caller walked away mid-upload.
	abortSafe(c, http.StatusBadRequest, "invalid_request_error")
	return nil, false
}

// installRequestBody makes payload the body every downstream reader sees,
// keeping the declared length consistent with the bytes now behind it.
func installRequestBody(request *http.Request, payload []byte) {
	request.Body = io.NopCloser(bytes.NewReader(payload))
	request.ContentLength = int64(len(payload))
	if request.Header.Get("Content-Length") != "" {
		request.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	}
}
