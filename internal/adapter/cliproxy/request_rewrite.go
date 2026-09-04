package cliproxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/clemsix6/LLMGW/internal/domain/contextedit"
	"github.com/clemsix6/LLMGW/internal/domain/effort"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/toolprefix"
	"github.com/gin-gonic/gin"
)

// maxRewriteBody bounds the payload LLMGW is willing to hold in memory for one
// request rewrite, in either direction. It sits above Anthropic's own
// practical request ceiling, so only a request that would fail upstream anyway
// ever reaches it.
const maxRewriteBody = 32 << 20

// requestRewrite is what one request's body needs before the SDK handler reads
// it. A zero value means nothing applies, which is the case for every request
// that is not a generation.
type requestRewrite struct {
	prefixToolNames   bool   // prefixToolNames rewrites the tool names the payload declares.
	effortLevel       string // effortLevel is the thinking effort to inject, empty meaning none.
	claimContextEdits bool   // claimContextEdits keeps context editing with the caller.
}

// engaged reports whether any transformation applies. A request none applies to
// keeps exactly today's path: its body is never read and nothing is allocated
// on its behalf.
func (r requestRewrite) engaged() bool {
	return r.prefixToolNames || r.effortLevel != "" || r.claimContextEdits
}

// apply runs every engaged transformation over one payload, in one pass over
// the bytes the body was read into.
func (r requestRewrite) apply(payload []byte) []byte {
	if r.prefixToolNames {
		payload = toolprefix.PrefixRequest(payload)
	}
	if r.claimContextEdits {
		payload = contextedit.Claim(payload)
	}
	return effort.Apply(payload, r.effortLevel)
}

// resolveRequestRewrite decides what this request needs. The tool-name rewrite
// applies to every project on both Anthropic payload routes, since count_tokens
// must count the payload actually sent; the effort injection and the
// context-editing claim cover generation alone — effort moves output tokens and
// cannot move the count count_tokens answers with, and only a generation
// carries a prompt cache to protect.
func resolveRequestRewrite(
	identity governance.KeyIdentity,
	request *http.Request,
) requestRewrite {
	if request.Method != http.MethodPost {
		return requestRewrite{}
	}
	rewrite := requestRewrite{}
	if request.URL.Path == messagesPath || request.URL.Path == countTokensPath {
		rewrite.prefixToolNames = true
	}
	if request.URL.Path == messagesPath {
		rewrite.effortLevel = identity.DefaultEffort
		rewrite.claimContextEdits = true
	}
	return rewrite
}

// rewriteRequest applies whatever this request needs to its body, before the
// SDK handler reads it. It reports false when the request was refused.
//
// The refusal releases the generation permit explicitly, the way the budget
// abort does: it returns before the barrier defer is registered, so nothing
// else gives the permit back, and the bridge's finite capacity would shrink
// for the life of the process.
func (m *Middleware) rewriteRequest(
	c *gin.Context,
	identity governance.KeyIdentity,
	requestID string,
	reserved bool,
) bool {
	rewrite := resolveRequestRewrite(identity, c.Request)
	if !rewrite.engaged() {
		return true
	}
	if rewriteRequestBody(c, rewrite) {
		return true
	}
	if reserved {
		m.bridge.release(requestID)
	}
	return false
}

// rewriteRequestBody reads the body once, transforms it, and installs the
// result, so the SDK handler reads the rewritten payload instead of the one
// the client sent. It reports false when the request was aborted, in which
// case the caller still owns whatever it reserved before calling.
func rewriteRequestBody(c *gin.Context, rewrite requestRewrite) bool {
	if c.Request.Body == nil {
		return true
	}
	payload, ok := readBoundedBody(c)
	if !ok {
		return false
	}
	installRequestBody(c.Request, rewrite.apply(payload))
	return true
}

// readBoundedBody reads the whole request body under the maxRewriteBody
// ceiling. A declared length above the ceiling is refused before a single byte
// is buffered; an undeclared one is caught by the bounded reader instead. It
// has already aborted the request when it reports false, so its caller writes
// no second response.
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
