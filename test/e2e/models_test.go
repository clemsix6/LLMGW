package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

// TestModelsEndpointReal boots the full gateway with the codex route wired and asserts
// GET /v1/models returns the OpenAI list envelope advertising every served Codex model. It is
// deterministic (the discovery endpoint never calls the provider), so it runs in the hermetic
// suite without real credentials — dummy credentials wire the route and are never used.
func TestModelsEndpointReal(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness, err := Start(ctx)
	if err != nil {
		t.Fatalf("start harness: %v", err)
	}
	t.Cleanup(func() { harness.Stop(context.Background()) })

	// accessToken="" skips token handling; the dummy refresh/account only wire the route.
	if err := harness.SeedCodex(ctx, "models-test", "dummy-refresh", "acct_dummy", testCodexVersion, ""); err != nil {
		t.Fatalf("seed codex route: %v", err)
	}

	body := getModels(t, ctx, harness)
	for _, model := range []string{"gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		assertAdvertisesModel(t, body, model)
	}
}

// getModels GETs /v1/models and returns the response body, failing on a non-200.
func getModels(t *testing.T, ctx context.Context, harness *Harness) []byte {
	t.Helper()

	resp, err := harness.Get(ctx, "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /v1/models body: %v", err)
	}
	return body
}

// assertAdvertisesModel asserts the body is the OpenAI list envelope and advertises id.
func assertAdvertisesModel(t *testing.T, body []byte, id string) {
	t.Helper()

	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode /v1/models (%v): %s", err, body)
	}
	if list.Object != "list" {
		t.Errorf("object = %q, want %q", list.Object, "list")
	}
	for _, m := range list.Data {
		if m.ID == id && m.Object == "model" {
			return
		}
	}
	t.Fatalf("model %q not advertised in /v1/models: %s", id, body)
}
