package toolprefix

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

// TestStripResponseRoundTripsPrefixRequest proves StripResponse reverses the
// exact name PrefixRequest produced.
func TestStripResponseRoundTripsPrefixRequest(t *testing.T) {
	request := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"search_web","input":{}}]}]}`)
	prefixed := PrefixRequest(request)
	prefixedName := gjson.GetBytes(prefixed, "messages.0.content.0.name").String()

	response := []byte(fmt.Sprintf(`{"type":"message","content":[{"type":"tool_use","name":%q,"input":{}}]}`, prefixedName))
	got := StripResponse(response)
	if v := gjson.GetBytes(got, "content.0.name").String(); v != "search_web" {
		t.Fatalf("content.0.name = %q, want search_web", v)
	}
}

// TestStripResponseLeavesNonToolUseBlocksUntouched proves a text block that
// happens to contain the prefix in its prose is never inspected.
func TestStripResponseLeavesNonToolUseBlocksUntouched(t *testing.T) {
	payload := []byte(`{"content":[{"type":"text","text":"hello new_search_web"}]}`)
	got := StripResponse(payload)
	if string(got) != string(payload) {
		t.Fatalf("text block changed: got %s, want %s", got, payload)
	}
}

// TestStripResponseLeavesUnprefixedNameUntouched proves a tool_use name that
// never carried the prefix is forwarded unchanged, never truncated.
func TestStripResponseLeavesUnprefixedNameUntouched(t *testing.T) {
	payload := []byte(`{"content":[{"type":"tool_use","name":"search_web","input":{}}]}`)
	got := StripResponse(payload)
	if v := gjson.GetBytes(got, "content.0.name").String(); v != "search_web" {
		t.Fatalf("content.0.name = %q, want search_web unchanged", v)
	}
}

// TestStripResponseMalformedJSONUnchanged proves a payload that is not valid
// JSON is returned unchanged rather than rejected.
func TestStripResponseMalformedJSONUnchanged(t *testing.T) {
	payload := []byte(`{"content": not valid`)
	got := StripResponse(payload)
	if string(got) != string(payload) {
		t.Fatalf("malformed payload changed: got %s, want %s", got, payload)
	}
}

// TestStripStreamEventContentBlockStart proves a content_block_start event
// naming a prefixed tool is stripped, with the event: line, the data:
// prefix, and the trailing delimiter preserved exactly.
func TestStripStreamEventContentBlockStart(t *testing.T) {
	event := []byte("event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"new_search_web","input":{}}}` +
		"\n\n")
	got := StripStreamEvent(event)

	data := dataJSON(t, got)
	if v := gjson.GetBytes(data, "content_block.name").String(); v != "search_web" {
		t.Fatalf("content_block.name = %q, want search_web", v)
	}
	if !bytes.HasPrefix(got, []byte("event: content_block_start\n")) {
		t.Fatalf("event: line not preserved: %s", got)
	}
	if !bytes.HasSuffix(got, []byte("\n\n")) {
		t.Fatalf("trailing delimiter not preserved: %s", got)
	}
}

// TestStripStreamEventIgnoresOtherEventTypes proves an event whose top-level
// type is not content_block_start passes through unchanged.
func TestStripStreamEventIgnoresOtherEventTypes(t *testing.T) {
	event := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1"}}` +
		"\n\n")
	got := StripStreamEvent(event)
	if string(got) != string(event) {
		t.Fatalf("non-content_block_start event changed: got %s, want %s", got, event)
	}
}

// TestStripStreamEventIgnoresTextContentBlock proves a content_block_start
// carrying text rather than a tool is left untouched.
func TestStripStreamEventIgnoresTextContentBlock(t *testing.T) {
	event := []byte("event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` +
		"\n\n")
	got := StripStreamEvent(event)
	if string(got) != string(event) {
		t.Fatalf("text content_block changed: got %s, want %s", got, event)
	}
}

// TestStripStreamEventIgnoresCommentFrameWithNoDataLine proves the SDK's
// keep-alive comment frame, which carries no data: line, survives untouched.
func TestStripStreamEventIgnoresCommentFrameWithNoDataLine(t *testing.T) {
	event := []byte(": keep-alive\n\n")
	got := StripStreamEvent(event)
	if string(got) != string(event) {
		t.Fatalf("keep-alive frame changed: got %s, want %s", got, event)
	}
}

// TestStripStreamEventLeavesUnprefixedNameUntouched proves a tool_use name
// that never carried the prefix is forwarded unchanged.
func TestStripStreamEventLeavesUnprefixedNameUntouched(t *testing.T) {
	event := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"search_web"}}` + "\n\n")
	got := StripStreamEvent(event)
	if string(got) != string(event) {
		t.Fatalf("unprefixed name changed: got %s, want %s", got, event)
	}
}

// TestStripStreamEventMalformedJSONUnchanged proves an event whose data: line
// is not valid JSON is returned unchanged rather than rejected.
func TestStripStreamEventMalformedJSONUnchanged(t *testing.T) {
	event := []byte("data: not valid json\n\n")
	got := StripStreamEvent(event)
	if string(got) != string(event) {
		t.Fatalf("malformed event changed: got %s, want %s", got, event)
	}
}

// dataJSON extracts the JSON value of an SSE event's data: line for
// assertions, failing the test if no such line exists.
func dataJSON(t *testing.T, event []byte) []byte {
	t.Helper()
	start, end, ok := dataLineBounds(event)
	if !ok {
		t.Fatalf("event has no data: line: %s", event)
	}
	return event[start:end]
}
