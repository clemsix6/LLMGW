package cliproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// plainGeneration is one generation body naming neither an effort nor a tool.
const plainGeneration = `{"model":"claude-opus-5","messages":[]}`

// TestRequestRewriteRouteEligibility proves each transformation reaches its own
// routes: the tool-name rewrite covers both Anthropic payload routes, while the
// effort injection stops at generation, and so does the context-editing claim:
// count_tokens must count the payload the client sent, and a field the gateway
// added would move that count. count_tokens is answered locally and issues no
// upstream call, so the eligibility predicate is the only place those
// exclusions have an observable.
func TestRequestRewriteRouteEligibility(t *testing.T) {
	identity := governance.KeyIdentity{PrefixToolNames: true, DefaultEffort: "high"}
	tests := []struct {
		name       string
		method     string
		path       string
		wantPrefix bool
		wantEffort string
		wantClaim  bool
	}{
		{
			name:       "messages",
			method:     http.MethodPost,
			path:       messagesPath,
			wantPrefix: true,
			wantEffort: "high",
			wantClaim:  true,
		},
		{
			name:       "count_tokens",
			method:     http.MethodPost,
			path:       countTokensPath,
			wantPrefix: true,
			wantEffort: "",
		},
		{
			name:   "models",
			method: http.MethodGet,
			path:   "/v1/models",
		},
		{
			name:   "another posted route",
			method: http.MethodPost,
			path:   "/v1/chat/completions",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			got := resolveRequestRewrite(identity, request)
			if got.prefixToolNames != test.wantPrefix ||
				got.effortLevel != test.wantEffort ||
				got.claimContextEdits != test.wantClaim {
				t.Fatalf("resolveRequestRewrite = %+v, want prefix %v effort %q claim %v",
					got, test.wantPrefix, test.wantEffort, test.wantClaim)
			}
		})
	}
}

// TestEffortInjectionReachesTheOutboundBody proves the level the authenticated
// project carries is what the SDK handler reads, and that a project without one
// contributes no effort of its own. Every generation also carries the
// context-editing claim, so the body is compared field by field rather than
// whole.
func TestEffortInjectionReachesTheOutboundBody(t *testing.T) {
	tests := []struct {
		name  string
		level string
		want  string
	}{
		{name: "with a level", level: "low", want: "low"},
		{name: "without a level", level: "", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keys := validKeys()
			keys.identity.DefaultEffort = test.level

			var seenBody string
			runMiddlewareRequest(t, middlewareRequest{
				method:   http.MethodPost,
				path:     messagesPath,
				headers:  validHeaders(),
				keys:     keys,
				requests: &fakeRequests{},
				body:     strings.NewReader(plainGeneration),
				next: func(c *gin.Context) {
					payload, _ := io.ReadAll(c.Request.Body)
					seenBody = string(payload)
					c.Status(http.StatusOK)
				},
			})

			got := gjson.Get(seenBody, "output_config.effort").String()
			if got != test.want {
				t.Fatalf("output_config.effort = %q, want %q", got, test.want)
			}
			if test.level == "" && gjson.Get(seenBody, "output_config").Exists() {
				t.Fatalf("handler read %s, want no output_config at all", seenBody)
			}
			assertClientFieldsIntact(t, seenBody, plainGeneration)
		})
	}
}

// TestDisabledThinkingKeepsTheClientEffort proves the gateway itself declines
// to inject into a request that turned thinking off, which on Opus 5 would be a
// 400 above effort high.
//
// It is asserted here rather than end to end because the embedded SDK deletes
// output_config.effort from any thinking-off request on its way upstream: the
// integration assertion would hold with the gateway's own guard removed, and
// only this one bites.
func TestDisabledThinkingKeepsTheClientEffort(t *testing.T) {
	const disabledThinking = `{"model":"claude-opus-5","messages":[],"thinking":{"type":"disabled"}}`

	keys := validKeys()
	keys.identity.DefaultEffort = "max"

	var seenBody string
	runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     messagesPath,
		headers:  validHeaders(),
		keys:     keys,
		requests: &fakeRequests{},
		body:     strings.NewReader(disabledThinking),
		next: func(c *gin.Context) {
			payload, _ := io.ReadAll(c.Request.Body)
			seenBody = string(payload)
			c.Status(http.StatusOK)
		},
	})

	if got := gjson.Get(seenBody, "output_config"); got.Exists() {
		t.Fatalf("handler read output_config %s, want none", got.Raw)
	}
	assertClientFieldsIntact(t, seenBody, disabledThinking)
}

// assertClientFieldsIntact proves every top-level field the client sent
// survived the rewrite with its value, which is what "the body was left alone"
// means once the gateway adds a field of its own to every generation.
func assertClientFieldsIntact(t *testing.T, seenBody, clientBody string) {
	t.Helper()
	for field, want := range gjson.Parse(clientBody).Map() {
		if got := gjson.Get(seenBody, field); got.Raw != want.Raw {
			t.Fatalf("handler read %s = %s, want %s", field, got.Raw, want.Raw)
		}
	}
}

// TestOneBodyReadCarriesBothTransformations proves a project that enabled both
// settings gets both applied to a single request, which is the property the
// generalized rewrite exists for: two independent rewrites would each read the
// body, and the second would read the one the first had already consumed.
func TestOneBodyReadCarriesBothTransformations(t *testing.T) {
	keys := validKeys()
	keys.identity.PrefixToolNames = true
	keys.identity.DefaultEffort = "high"

	var seenBody string
	runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     messagesPath,
		headers:  validHeaders(),
		keys:     keys,
		requests: &fakeRequests{},
		body:     strings.NewReader(`{"tools":[{"name":"search_web"}],"messages":[]}`),
		next: func(c *gin.Context) {
			payload, _ := io.ReadAll(c.Request.Body)
			seenBody = string(payload)
			c.Status(http.StatusOK)
		},
	})

	if name := gjson.Get(seenBody, "tools.0.name").String(); name != "new_search_web" {
		t.Fatalf("tools.0.name = %q, want new_search_web", name)
	}
	if level := gjson.Get(seenBody, "output_config.effort").String(); level != "high" {
		t.Fatalf("output_config.effort = %q, want high", level)
	}
}

// TestOversizedEffortOnlyRequestReturnsThePermit proves the 32 MiB refusal now
// covers a project that enabled the effort alone, and still gives the
// generation permit back. The bridge's capacity is finite and a leaked permit
// is never recovered, so enough refusals would poison it and stop the service.
func TestOversizedEffortOnlyRequestReturnsThePermit(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	keys := validKeys()
	keys.identity.DefaultEffort = "medium"

	var nextCalled bool
	refused := runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     messagesPath,
		headers:  validHeaders(),
		keys:     keys,
		requests: &fakeRequests{},
		bridge:   bridge,
		body:     io.LimitReader(endlessReader{}, maxRewriteBody+1),
		next:     func(*gin.Context) { nextCalled = true },
	})

	if refused.Code != http.StatusRequestEntityTooLarge || nextCalled {
		t.Fatalf("status = %d next = %v, want 413 and no next", refused.Code, nextCalled)
	}
	if bridge.outstanding() != 0 {
		t.Fatalf("bridge holds %d permits after a refusal, want 0", bridge.outstanding())
	}

	// The capacity is genuinely back: the same one-permit bridge admits again.
	admitted := runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     messagesPath,
		headers:  validHeaders(),
		keys:     keys,
		requests: &fakeRequests{},
		bridge:   bridge,
		body:     strings.NewReader(plainGeneration),
	})
	if admitted.Code != http.StatusOK {
		t.Fatalf("status = %d after the refusal, want 200 from a restored bridge", admitted.Code)
	}
}

// TestRewriteRequestBodyAcceptsAbsentBody proves an engaged rewrite over a
// request carrying no body at all returns without touching it, rather than
// installing an empty one or panicking on the nil reader.
func TestRewriteRequestBodyAcceptsAbsentBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, messagesPath, nil)
	c.Request.Body = nil

	if !rewriteRequestBody(c, requestRewrite{effortLevel: "low"}) {
		t.Fatal("rewriteRequestBody = false for an absent body, want it accepted")
	}
	if c.Request.Body != nil {
		t.Fatal("an absent body was replaced, want it left absent")
	}
}
