package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/tidwall/gjson"
)

// TestDefaultEffortOutboundInjection proves the four trigger conditions of
// spec §5 on the Anthropic-format path, where the executor forwards the
// payload essentially untouched and the injected field is literally what the
// upstream receives.
func TestDefaultEffortOutboundInjection(t *testing.T) {
	t.Run("no default leaves the payload alone", func(t *testing.T) {
		created := testHarness.createKey(t, "effort-outbound-none")
		effortGeneration(t, created, effortPlainBody)
		assertUpstreamEffort(t, "")
	})

	t.Run("a default reaches the upstream", func(t *testing.T) {
		created := testHarness.createKey(t, "effort-outbound-set")
		testHarness.setDefaultEffort(t, created, "medium")
		effortGeneration(t, created, effortPlainBody)
		assertUpstreamEffort(t, "medium")
	})

	t.Run("the client's own effort wins", func(t *testing.T) {
		created := testHarness.createKey(t, "effort-outbound-client")
		testHarness.setDefaultEffort(t, created, "max")
		effortGeneration(t, created, effortClientLevelBody)
		assertUpstreamEffort(t, "low")
	})

	// This case documents the end-to-end outcome; it does not prove the gateway
	// produced it. The embedded SDK deletes output_config.effort from any
	// thinking-off request on its way upstream, so the assertion below holds
	// even with the gateway's own guard removed. What proves the guard is
	// TestDisabledThinkingKeepsTheClientBody, in internal/adapter/cliproxy,
	// which reads the body before the SDK touches it.
	t.Run("disabled thinking is left alone", func(t *testing.T) {
		created := testHarness.createKey(t, "effort-outbound-disabled")
		testHarness.setDefaultEffort(t, created, "max")
		effortGeneration(t, created, effortDisabledThinkingBody)
		assertUpstreamEffort(t, "")
	})
}

// TestDefaultEffortAndToolPrefixShareOneBodyRead proves a project enabling both
// settings gets both applied to a single request: the upstream sees the
// namespaced tool name and the injected level together, which no arrangement
// of two independent body reads could produce.
func TestDefaultEffortAndToolPrefixShareOneBodyRead(t *testing.T) {
	created := testHarness.createKey(t, "effort-with-tool-prefix")
	testHarness.enableToolPrefix(t, created)
	testHarness.setDefaultEffort(t, created, "high")

	effortGeneration(t, created, effortToolBody)

	body := lastUpstreamBody(t)
	if name := gjson.GetBytes(body, "tools.0.name").String(); name != "new_search_web" {
		t.Fatalf("upstream tools.0.name = %q, want new_search_web", name)
	}
	if level := gjson.GetBytes(body, "output_config.effort").String(); level != "high" {
		t.Fatalf("upstream output_config.effort = %q, want high", level)
	}
}

// effortGeneration drives one generation through the claude-format provider
// and fails the test unless the gateway answered a plausible Anthropic message.
func effortGeneration(t *testing.T, created governance.CreatedKey, payload string) {
	t.Helper()
	testHarness.Upstream.Enqueue(anthropicStubResponse())
	status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(payload),
		requestHeaders{authorization: "Bearer " + created.Plaintext})
	if status != http.StatusOK {
		t.Fatalf("generation status = %d, want 200; body=%s", status, safeBodySummary(body))
	}
	assertAnthropicMessageShape(t, decodeJSON(t, body))
}

// setDefaultEffort sets one project's level through the real store method, so
// the suite exercises the operator surface rather than raw SQL fixtures.
func (h *Harness) setDefaultEffort(t *testing.T, created governance.CreatedKey, level string) {
	t.Helper()
	err := h.Store.SetProjectDefaultEffort(context.Background(), created.Key.ProjectName, level)
	if err != nil {
		t.Fatal("set integration project default effort failed")
	}
}

// assertUpstreamEffort checks the level the upstream received, with the empty
// string standing for a payload carrying no output_config.effort at all.
func assertUpstreamEffort(t *testing.T, want string) {
	t.Helper()
	got := gjson.GetBytes(lastUpstreamBody(t), "output_config.effort")
	if got.String() != want {
		t.Fatalf("upstream output_config.effort = %q, want %q", got.String(), want)
	}
	if want == "" && got.Exists() {
		t.Fatalf("upstream payload carries output_config.effort, want none")
	}
}

// lastUpstreamBody returns the most recently captured upstream request body.
func lastUpstreamBody(t *testing.T) []byte {
	t.Helper()
	bodies := testHarness.Upstream.Bodies()
	if len(bodies) == 0 {
		t.Fatal("upstream captured no request body")
	}
	return bodies[len(bodies)-1]
}
