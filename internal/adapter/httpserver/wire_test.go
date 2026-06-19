package httpserver

import (
	"testing"
)

// TestOpenAIWireParseExtractsModelAndStream verifies that Parse extracts model and stream from
// a Chat Completions body and returns the raw body unchanged.
func TestOpenAIWireParseExtractsModelAndStream(t *testing.T) {
	body := []byte(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	req, err := OpenAIWire{}.Parse(body)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if req.Model() != "gpt-5" {
		t.Errorf("Model() = %q, want %q", req.Model(), "gpt-5")
	}
	if !req.Stream() {
		t.Errorf("Stream() = false, want true")
	}
	if string(req.Bytes()) != string(body) {
		t.Errorf("Bytes() = %q, want %q", req.Bytes(), body)
	}
}

// TestOpenAIWireParseStreamDefaultsFalse verifies that the stream field defaults to false
// when absent from the body.
func TestOpenAIWireParseStreamDefaultsFalse(t *testing.T) {
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)

	req, err := OpenAIWire{}.Parse(body)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if req.Stream() {
		t.Errorf("Stream() = true, want false when stream is absent")
	}
}

// TestOpenAIWireParseRejectsInvalidJSON verifies that Parse returns an error for malformed input.
func TestOpenAIWireParseRejectsInvalidJSON(t *testing.T) {
	_, err := OpenAIWire{}.Parse([]byte(`not json`))
	if err == nil {
		t.Error("Parse returned nil error for invalid JSON, want error")
	}
}

// TestOpenAIWireDefaultTag verifies that DefaultTag returns the expected tag for the codex surface.
func TestOpenAIWireDefaultTag(t *testing.T) {
	if got := (OpenAIWire{}).DefaultTag(); got != "agentic" {
		t.Errorf("DefaultTag() = %q, want %q", got, "agentic")
	}
}
