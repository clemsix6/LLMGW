package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestModelsEndpointListsSortedUniqueModels verifies GET /v1/models returns the OpenAI list
// envelope with the sorted, deduplicated union of every route's advertised models, so an
// OpenAI-compatible discovery client sees each served model id exactly once.
func TestModelsEndpointListsSortedUniqueModels(t *testing.T) {
	routes := []Route{
		{Path: "/v1/messages"}, // advertises nothing
		{Path: "/v1/chat/completions", Models: []string{"gpt-5.5"}}, // the OpenAI surface
		{Path: "/v1/other", Models: []string{"gpt-5.5", "alpha"}},   // dup across routes + ordering
	}

	rec := httptest.NewRecorder()
	modelsHandler(routes)(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Object != "list" {
		t.Fatalf("object = %q, want %q", got.Object, "list")
	}

	ids := make([]string, 0, len(got.Data))
	for _, m := range got.Data {
		if m.Object != "model" {
			t.Fatalf("entry object = %q, want %q", m.Object, "model")
		}
		ids = append(ids, m.ID)
	}
	want := []string{"alpha", "gpt-5.5"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}
