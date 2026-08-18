package cliproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/gin-gonic/gin"
)

// guardedWriter builds one wrapped recorder ready for guard writer tests.
func guardedWriter(t *testing.T) (*gin.Context, *httptest.ResponseRecorder, *markupGuardWriter) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writer := newMarkupGuardWriter(c.Writer, 7, "req-guard")
	c.Writer = writer
	return c, recorder, writer
}

// cleanToolUseBody is a screened response carrying no leaked markup.
const cleanToolUseBody = `{"content":[{"type":"tool_use","id":"t1","name":"submit_post",` +
	`"input":{"title":"Fine title","body":"Fine body"}}]}`

// leakedToolUseBody is a screened response carrying a production leak shape.
const leakedToolUseBody = `{"content":[{"type":"tool_use","id":"t1","name":"submit_post",` +
	`"input":{"title":"</antml railway> <parameter name=\"body\">Druck"}}]}`

func TestMarkupGuardWriterForwardsCleanBufferedResponse(t *testing.T) {
	c, recorder, writer := guardedWriter(t)
	c.Writer.Header().Set("Content-Type", "application/json")

	if _, err := writer.Write([]byte(cleanToolUseBody)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	writer.finalize()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if recorder.Body.String() != cleanToolUseBody {
		t.Fatalf("body = %q, want the screened body unchanged", recorder.Body.String())
	}
}

func TestMarkupGuardWriterRejectsLeakedBufferedResponse(t *testing.T) {
	c, recorder, writer := guardedWriter(t)
	c.Writer.Header().Set("Content-Type", "application/json")

	if _, err := writer.Write([]byte(leakedToolUseBody)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	writer.finalize()

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
	if recorder.Body.String() != guardRejectionBody {
		t.Fatalf("body = %q, want the rejection envelope", recorder.Body.String())
	}
}

func TestMarkupGuardWriterPassesStreamsThrough(t *testing.T) {
	c, recorder, writer := guardedWriter(t)
	c.Writer.Header().Set("Content-Type", "text/event-stream")

	event := "data: {\"leak\":\"</antml>\"}\n\n"
	if _, err := writer.Write([]byte(event)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	writer.finalize()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if recorder.Body.String() != event {
		t.Fatalf("body = %q, want the stream forwarded verbatim", recorder.Body.String())
	}
}

func TestMarkupGuardWriterPassesErrorBodiesThrough(t *testing.T) {
	_, recorder, writer := guardedWriter(t)

	writer.WriteHeader(http.StatusBadRequest)
	errorBody := `{"error":{"type":"invalid_request_error"}}`
	if _, err := writer.Write([]byte(errorBody)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	writer.finalize()

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if recorder.Body.String() != errorBody {
		t.Fatalf("body = %q, want the error body unchanged", recorder.Body.String())
	}
}

func TestMarkupGuardResponsePredicate(t *testing.T) {
	flagged := governance.KeyIdentity{RejectToolMarkup: true}
	unflagged := governance.KeyIdentity{}

	generation, _ := http.NewRequest(http.MethodPost, messagesPath, nil)
	countTokens, _ := http.NewRequest(http.MethodPost, countTokensPath, nil)
	models, _ := http.NewRequest(http.MethodGet, "/v1/models", nil)

	if !markupGuardResponse(flagged, generation) {
		t.Fatal("flagged generation request must be screened")
	}
	if markupGuardResponse(unflagged, generation) {
		t.Fatal("unflagged generation request must not be screened")
	}
	if markupGuardResponse(flagged, countTokens) {
		t.Fatal("count_tokens must not be screened")
	}
	if markupGuardResponse(flagged, models) {
		t.Fatal("metadata routes must not be screened")
	}
}
