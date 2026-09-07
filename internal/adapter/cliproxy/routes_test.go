package cliproxy

import "testing"

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

// TestNoRouteServedSurfacesAreDenied pins the invariant routed() rests on: the
// SDK answers a few paths from its own NoRoute handler instead of from the
// route table, so they carry no matched route and read here as unrouted. That
// only stays correct while every one of them is a surface LLMGW denies before
// routing is ever consulted. These are the ones the pinned SDK serves that way;
// an SDK upgrade that serves anything else from NoRoute has to be classified in
// deniedPath, or it will be refused instead of reaching its handler.
func TestNoRouteServedSurfacesAreDenied(t *testing.T) {
	noRouteServed := []string{
		"/v0/management",
		"/v0/management/config",
		"/v0/resource/plugins/any-resource",
	}

	for _, path := range noRouteServed {
		for _, method := range []string{"GET", "POST"} {
			if got := Classify(method, path); got != RouteDenied {
				t.Errorf("Classify(%q, %q) = %d, want denied", method, path, got)
			}
		}
	}
}
