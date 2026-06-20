package httpserver

import (
	"encoding/json"
	"net/http"
	"sort"
)

// modelObject is one entry in the OpenAI GET /v1/models response.
type modelObject struct {
	ID      string `json:"id"`       // ID is the model identifier clients pass as "model".
	Object  string `json:"object"`   // Object is always "model".
	OwnedBy string `json:"owned_by"` // OwnedBy is the nominal owner of the model.
}

// modelList is the OpenAI GET /v1/models response envelope.
type modelList struct {
	Object string        `json:"object"` // Object is always "list".
	Data   []modelObject `json:"data"`   // Data is the advertised models.
}

// modelsHandler serves GET /v1/models with the deduplicated, sorted union of every route's
// advertised models, in the OpenAI list format. OpenAI-compatible discovery clients (e.g. a
// model picker) call this to learn which model ids the gateway serves.
func modelsHandler(routes []Route) http.HandlerFunc {
	models := advertisedModels(routes)
	return func(w http.ResponseWriter, _ *http.Request) {
		list := modelList{Object: "list", Data: make([]modelObject, 0, len(models))}
		for _, id := range models {
			list.Data = append(list.Data, modelObject{ID: id, Object: "model", OwnedBy: "llmgw"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}
}

// advertisedModels returns the sorted, deduplicated model ids across all routes.
func advertisedModels(routes []Route) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, rt := range routes {
		for _, model := range rt.Models {
			if !seen[model] {
				seen[model] = true
				out = append(out, model)
			}
		}
	}
	sort.Strings(out)
	return out
}
