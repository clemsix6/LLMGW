package cliproxy

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   RouteClass
	}{
		{"GET", "/healthz", RoutePublic},
		{"HEAD", "/healthz", RoutePublic},
		{"GET", "/", RouteDenied},
		{"GET", "/management.html", RouteDenied},
		{"GET", "/v0/management/config", RouteDenied},
		{"GET", "/v0/resource/plugins/x", RouteDenied},
		{"GET", "/anthropic/callback", RouteDenied},
		{"GET", "/codex/callback", RouteDenied},
		{"GET", "/antigravity/callback", RouteDenied},
		{"GET", "/v1/models", RouteMetadata},
		{"GET", "/v1beta/models", RouteMetadata},
		{"GET", "/v1beta/models/gemini-test", RouteMetadata},
		{"POST", "/v1/messages/count_tokens", RouteMetadata},
		{"POST", "/v1beta/models/gemini-test:countTokens", RouteMetadata},
		{"POST", "/v1/messages", RouteGeneration},
		{"POST", "/v1/responses", RouteGeneration},
		{"GET", "/new-sdk-route", RouteGeneration},
	}

	for _, test := range tests {
		name := test.method + "_" + test.path
		t.Run(name, func(t *testing.T) {
			if got := Classify(test.method, test.path); got != test.want {
				t.Fatalf("Classify(%q, %q) = %d, want %d", test.method, test.path, got, test.want)
			}
		})
	}
}

func TestClassifyDoesNotBroadenSpecialRoutes(t *testing.T) {
	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/healthz"},
		{"GET", "/healthz/more"},
		{"GET", "/v1/messages/count_tokens"},
		{"POST", "/v1beta/models/gemini-test:generateContent"},
	}

	for _, test := range tests {
		if got := Classify(test.method, test.path); got != RouteGeneration {
			t.Fatalf("Classify(%q, %q) = %d, want generation", test.method, test.path, got)
		}
	}
}
