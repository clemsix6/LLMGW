package cliproxy

import (
	"net/http"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/gin-gonic/gin"
)

const (
	// messagesPath is the Anthropic generation route, the only one whose
	// response can carry tool names back to the client.
	messagesPath = "/v1/messages"
	// countTokensPath is the Anthropic token-count route. It carries the same
	// payload shape as a generation, so counting a payload that differs from
	// the one actually sent would hand the client a number it cannot reconcile
	// with its bill.
	countTokensPath = "/v1/messages/count_tokens"
)

// toolPrefixRequest reports whether this request's body carries the tool names
// a flagged project namespaces. A project without the flag never matches, so
// its path stays exactly today's: the body is not read and nothing is
// allocated on its behalf.
func toolPrefixRequest(identity governance.KeyIdentity, request *http.Request) bool {
	if !identity.PrefixToolNames || request.Method != http.MethodPost {
		return false
	}
	return request.URL.Path == messagesPath || request.URL.Path == countTokensPath
}

// toolPrefixResponse reports whether this request's response can carry
// namespaced tool names back. count_tokens answers with a token count alone,
// so it takes the request rewrite and no wrapper.
func toolPrefixResponse(identity governance.KeyIdentity, request *http.Request) bool {
	if !identity.PrefixToolNames || request.Method != http.MethodPost {
		return false
	}
	return request.URL.Path == messagesPath
}

// rewriteToolNames namespaces the tool names a flagged project declares,
// before the SDK handler reads the body. It reports false when the request was
// refused.
//
// The refusal releases the generation permit explicitly, the way the budget
// abort a few lines above does: it returns before the barrier defer is
// registered, so nothing else gives the permit back, and the bridge's finite
// capacity would shrink for the life of the process.
func (m *Middleware) rewriteToolNames(
	c *gin.Context,
	identity governance.KeyIdentity,
	requestID string,
	reserved bool,
) bool {
	if !toolPrefixRequest(identity, c.Request) {
		return true
	}
	if rewriteRequestBody(c) {
		return true
	}
	if reserved {
		m.bridge.release(requestID)
	}
	return false
}

// installToolPrefixWriter wraps the response writer of a flagged project's
// generation request and returns the finalization its caller must defer. It
// returns nil for every request that needs no rewrite, which is what keeps an
// unflagged project free of any wrapper or buffering.
func installToolPrefixWriter(c *gin.Context, identity governance.KeyIdentity) func() {
	if !toolPrefixResponse(identity, c.Request) {
		return nil
	}
	writer := newToolPrefixWriter(c.Writer)
	c.Writer = writer
	return writer.finalize
}
