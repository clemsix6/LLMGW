package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// StubResponse describes one deterministic upstream response.
type StubResponse struct {
	Status           int             // Status is the response status code.
	Headers          http.Header     // Headers contains response headers copied to the client.
	Body             string          // Body contains a JSON or SSE fixture.
	Chunks           []StubChunk     // Chunks optionally streams flushed response fragments.
	Started          chan<- struct{} // Started receives when the upstream handler begins.
	Release          <-chan struct{} // Release optionally blocks the upstream response.
	CloseConnection  bool            // CloseConnection drops the transport before response headers.
	CloseAfterChunks bool            // CloseAfterChunks drops the transport after flushed fragments.
}

// StubChunk is one flushed upstream fragment with an optional post-flush gate.
type StubChunk struct {
	Body    string          // Body is written as one fragment.
	Flushed chan<- struct{} // Flushed receives after the fragment is visible.
	Release <-chan struct{} // Release blocks the next fragment/handler return.
}

// StubUpstream is a scriptable OpenAI-compatible test server.
type StubUpstream struct {
	server *httptest.Server // server owns the local HTTP listener.

	mu             sync.Mutex     // mu protects scripts, authorizations, and bodies.
	scripts        []StubResponse // scripts contains responses in request order.
	authorizations []string       // authorizations captures selected upstream credentials.
	bodies         [][]byte       // bodies captures each upstream request payload, in arrival order.
	statuses       []int          // statuses captures every scripted upstream status.
	active         int            // active counts upstream handlers that have not returned.
}

// NewStubUpstream starts a deterministic local provider.
func NewStubUpstream() *StubUpstream {
	upstream := &StubUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(upstream.serveHTTP))
	return upstream
}

// URL returns the local upstream base URL.
func (s *StubUpstream) URL() string {
	return s.server.URL
}

// Enqueue appends scripted JSON or SSE responses.
func (s *StubUpstream) Enqueue(responses ...StubResponse) {
	s.mu.Lock()
	s.scripts = append(s.scripts, responses...)
	s.mu.Unlock()
}

// Close stops the local upstream.
func (s *StubUpstream) Close() {
	if s != nil && s.server != nil {
		s.server.Close()
	}
}

// Bodies returns every request body the upstream captured, in arrival order.
func (s *StubUpstream) Bodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

// serveHTTP captures account selection, the request body, and returns the next fixture.
func (s *StubUpstream) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	response := s.nextResponse(request.Header.Get("Authorization"), body)
	defer s.finishRequest()
	if response.Started != nil {
		response.Started <- struct{}{}
	}
	if response.Release != nil {
		<-response.Release
	}
	if response.CloseConnection {
		s.closeConnection(writer)
		return
	}
	for name, values := range response.Headers {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	if response.Status == 0 {
		response.Status = http.StatusOK
	}
	s.recordStatus(response.Status)
	writer.WriteHeader(response.Status)
	if len(response.Chunks) > 0 {
		flusher, _ := writer.(http.Flusher)
		for _, chunk := range response.Chunks {
			_, _ = writer.Write([]byte(chunk.Body))
			if flusher != nil {
				flusher.Flush()
			}
			if chunk.Flushed != nil {
				chunk.Flushed <- struct{}{}
			}
			if chunk.Release != nil {
				<-chunk.Release
			}
		}
		if response.CloseAfterChunks {
			s.closeConnection(writer)
		}
		return
	}
	_, _ = writer.Write([]byte(response.Body))
}

// recordStatus captures one resolved status without exposing response content.
func (s *StubUpstream) recordStatus(status int) {
	s.mu.Lock()
	s.statuses = append(s.statuses, status)
	s.mu.Unlock()
}

// closeConnection abruptly closes one HTTP/1 upstream connection.
func (s *StubUpstream) closeConnection(writer http.ResponseWriter) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		return
	}
	connection, _, err := hijacker.Hijack()
	if err == nil {
		_ = connection.Close()
	}
}

// nextResponse atomically captures authorization and body, then consumes one script.
func (s *StubUpstream) nextResponse(authorization string, body []byte) StubResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.active++
	s.authorizations = append(s.authorizations, authorization)
	s.bodies = append(s.bodies, body)
	if len(s.scripts) == 0 {
		return defaultStubResponse()
	}
	response := s.scripts[0]
	s.scripts = s.scripts[1:]
	return response
}

// finishRequest records that one upstream handler returned.
func (s *StubUpstream) finishRequest() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
}

// defaultStubResponse returns a valid non-streaming OpenAI completion.
func defaultStubResponse() StubResponse {
	return StubResponse{
		Status: http.StatusOK,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: `{
			"id":"chatcmpl-integration",
			"object":"chat.completion",
			"created":1,
			"model":"upstream-model",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"fixture-response"},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
		}`,
	}
}

// sseFrame is one fully delimited server-sent event.
type sseFrame struct {
	Event string         // Event is the optional protocol event name.
	Data  map[string]any // Data is the decoded JSON payload.
	Done  bool           // Done identifies OpenAI's terminal sentinel.
}

// validateProtocolStream enforces the downstream framing contract for one protocol.
func validateProtocolStream(protocol string, body []byte) error {
	frames, err := parseSSEFrames(body)
	if err != nil {
		return err
	}
	switch protocol {
	case "anthropic messages":
		return validateNamedSSE(frames, "message_start", "message_stop")
	case "openai responses":
		return validateNamedSSE(frames, "response.created", "response.completed")
	case "openai chat completions":
		return validateChatSSE(frames)
	default:
		return fmt.Errorf("unknown stream protocol %q", protocol)
	}
}

// parseSSEFrames rejects incomplete, concatenated, or non-JSON event frames.
func parseSSEFrames(body []byte) ([]sseFrame, error) {
	text := string(body)
	if text == "" || !strings.HasSuffix(text, "\n\n") || strings.Contains(text, "\r") {
		return nil, errors.New("stream is not LF-delimited SSE")
	}
	var frames []sseFrame
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			lines = append(lines, line)
			continue
		}
		if len(lines) == 0 {
			continue
		}
		frame, err := parseSSEFrame(strings.Join(lines, "\n"))
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
		lines = nil
	}
	return frames, nil
}

// parseSSEFrame decodes one event with exactly one data field.
func parseSSEFrame(raw string) (sseFrame, error) {
	var frame sseFrame
	var data string
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			if frame.Event != "" {
				return sseFrame{}, errors.New("duplicate event field")
			}
			frame.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if data != "" {
				return sseFrame{}, errors.New("duplicate data field")
			}
			data = strings.TrimPrefix(line, "data: ")
		default:
			return sseFrame{}, fmt.Errorf("malformed SSE field %q", line)
		}
	}
	if data == "[DONE]" {
		frame.Done = true
		return frame, nil
	}
	if data == "" || json.Unmarshal([]byte(data), &frame.Data) != nil {
		return sseFrame{}, errors.New("invalid SSE JSON payload")
	}
	return frame, nil
}

// validateNamedSSE enforces event/payload identity, ordering, and one terminal event.
func validateNamedSSE(frames []sseFrame, first string, terminal string) error {
	if len(frames) < 2 || frames[0].Event != first || frames[len(frames)-1].Event != terminal {
		return errors.New("named SSE event order is invalid")
	}
	terminalCount := 0
	for _, frame := range frames {
		if frame.Done || frame.Event == "" || frame.Data["type"] != frame.Event {
			return errors.New("named SSE event and payload disagree")
		}
		if frame.Event == terminal {
			terminalCount++
		}
	}
	if terminalCount != 1 {
		return errors.New("named SSE terminal count is invalid")
	}
	return nil
}

// validateChatSSE enforces JSON chunks followed by exactly one final sentinel.
func validateChatSSE(frames []sseFrame) error {
	if len(frames) < 2 || !frames[len(frames)-1].Done {
		return errors.New("chat SSE terminal order is invalid")
	}
	doneCount := 0
	for index, frame := range frames {
		if frame.Done {
			doneCount++
			if index != len(frames)-1 || frame.Event != "" {
				return errors.New("chat SSE terminal is not final")
			}
			continue
		}
		if frame.Event != "" || frame.Data["object"] != "chat.completion.chunk" {
			return errors.New("chat SSE payload is invalid")
		}
	}
	if doneCount != 1 {
		return errors.New("chat SSE terminal count is invalid")
	}
	return nil
}
