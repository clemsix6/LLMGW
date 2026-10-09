package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// haikuCatalogEntry is the upstream catalog's claude-haiku-5-5 entry, copied
// from the router-for-me/models catalog the SDK refreshes from at runtime.
const haikuCatalogEntry = `{
	"id": "claude-haiku-5-5",
	"native_capabilities": {"web_search": true},
	"object": "model",
	"created": 1790553600,
	"owned_by": "anthropic",
	"type": "claude",
	"display_name": "Claude Haiku 5.5",
	"description": "For high-volume, latency-sensitive tasks such as classification, extraction, and routing.",
	"context_length": 1000000,
	"max_completion_tokens": 128000,
	"thinking": {
		"zero_allowed": true,
		"dynamic_allowed": true,
		"levels": ["low", "medium", "high", "xhigh", "max"]
	},
	"supportedInputModalities": ["text", "image"],
	"supportedOutputModalities": ["text"]
}`

// catalogRouteDeadline bounds the wait for the SDK's asynchronous first
// catalog refresh to publish the local catalog.
const catalogRouteDeadline = 30 * time.Second

// writeModelCatalog writes the catalog the harness points models.catalog at:
// the SDK's embedded catalog plus the upstream claude-haiku-5-5 entry, so the
// model is routable only through the runtime catalog refresh.
func writeModelCatalog(path string) error {
	sdkDir, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/router-for-me/CLIProxyAPI/v8").Output()
	if err != nil {
		return fmt.Errorf("locate the embedded SDK sources:\n%w", err)
	}
	embedded, err := os.ReadFile(filepath.Join(
		strings.TrimSpace(string(sdkDir)), "internal", "registry", "models", "models.json"))
	if err != nil {
		return fmt.Errorf("read the SDK embedded model catalog:\n%w", err)
	}

	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(embedded, &catalog); err != nil {
		return fmt.Errorf("parse the SDK embedded model catalog:\n%w", err)
	}
	var claude []json.RawMessage
	if err := json.Unmarshal(catalog["claude"], &claude); err != nil {
		return fmt.Errorf("parse the claude catalog section:\n%w", err)
	}
	claude = append(claude, json.RawMessage(haikuCatalogEntry))
	if catalog["claude"], err = json.Marshal(claude); err != nil {
		return fmt.Errorf("encode the claude catalog section:\n%w", err)
	}

	data, err := json.Marshal(catalog)
	if err != nil {
		return fmt.Errorf("encode the model catalog:\n%w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write the model catalog:\n%w", err)
	}
	return nil
}

// catalogMessage sends one /v1/messages generation for the model.
func catalogMessage(t *testing.T, key, model string) (int, []byte) {
	t.Helper()
	payload := `{"model": "` + model + `", "max_tokens": 64,
		"messages": [{"role": "user", "content": "fixture-prompt"}]}`
	return gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(payload), requestHeaders{authorization: "Bearer " + key})
}

// TestRuntimeCatalogMakesClaudeHaikuRoutable proves a model the SDK's embedded
// catalog lacks routes once the runtime catalog lists it, and that the model id
// reaches the upstream verbatim. The catalog is a local file, so the SDK's
// asynchronous first refresh is the only thing the test waits for.
func TestRuntimeCatalogMakesClaudeHaikuRoutable(t *testing.T) {
	created := testHarness.createKey(t, "catalog-haiku-5-5")

	deadline := time.Now().Add(catalogRouteDeadline)
	for {
		testHarness.Upstream.Enqueue(anthropicStubResponse())
		status, body := catalogMessage(t, created.Plaintext, "claude-haiku-5-5")
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("claude-haiku-5-5 status = %d after %s, want 200; body=%s",
				status, catalogRouteDeadline, safeBodySummary(body))
		}
		time.Sleep(250 * time.Millisecond)
	}

	if model := gjson.GetBytes(lastUpstreamBody(t), "model").String(); model != "claude-haiku-5-5" {
		t.Fatalf("upstream model = %q, want claude-haiku-5-5", model)
	}
}

// TestModelAbsentFromCatalogIsRefused proves routing follows the catalog: a
// model neither the embedded nor the runtime catalog lists is refused before
// any upstream is reached.
func TestModelAbsentFromCatalogIsRefused(t *testing.T) {
	created := testHarness.createKey(t, "catalog-unknown-model")

	status, body := catalogMessage(t, created.Plaintext, "claude-haiku-9-9")
	if status != http.StatusBadRequest {
		t.Fatalf("unlisted model status = %d, want 400; body=%s", status, safeBodySummary(body))
	}
}
