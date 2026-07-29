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
