package cliproxy

import (
	"net/http"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/gin-gonic/gin"
)

// markupGuardResponse reports whether this request's response must be screened
// for leaked tool-call markup. Only a generation response can carry tool_use
// blocks, so count_tokens and metadata routes are never wrapped.
func markupGuardResponse(identity governance.KeyIdentity, request *http.Request) bool {
	if !identity.RejectToolMarkup || request.Method != http.MethodPost {
		return false
	}
	return request.URL.Path == messagesPath
}

// installMarkupGuard wraps the response writer of a flagged project's
// generation request and returns the finalization its caller must defer. It
// returns nil for every request that needs no screening, which is what keeps
// an unflagged project free of any wrapper or buffering.
func installMarkupGuard(
	c *gin.Context,
	identity governance.KeyIdentity,
	requestID string,
) func() {
	if !markupGuardResponse(identity, c.Request) {
		return nil
	}
	writer := newMarkupGuardWriter(c.Writer, identity.ProjectID, requestID)
	c.Writer = writer
	return writer.finalize
}
