package cliproxy

import (
	"net/http"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/gin-gonic/gin"
)

const (
	// messagesPath is the Anthropic generation route, the only one whose
	// response can carry tool names back to the client and the only one an
	// effort level has any effect on.
	messagesPath = "/v1/messages"
	// countTokensPath is the Anthropic token-count route. It carries the same
	// payload shape as a generation, so counting a payload that differs from
	// the one actually sent would hand the client a number it cannot reconcile
	// with its bill.
	countTokensPath = "/v1/messages/count_tokens"
)

// toolPrefixResponse reports whether this request's response can carry
// namespaced tool names back. count_tokens answers with a token count alone,
// so it takes the request rewrite and no wrapper.
func toolPrefixResponse(identity governance.KeyIdentity, request *http.Request) bool {
	if !identity.PrefixToolNames || request.Method != http.MethodPost {
		return false
	}
	return request.URL.Path == messagesPath
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
