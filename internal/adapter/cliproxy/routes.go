package cliproxy

import (
	"net/http"
	"strings"
)

// RouteClass identifies the governance policy applied to one proxy route.
type RouteClass uint8

const (
	// RouteGeneration identifies an authenticated and metered generation route.
	RouteGeneration RouteClass = iota
	// RoutePublic identifies a route that does not require project authentication.
	RoutePublic
	// RouteDenied identifies a route that is never exposed by LLMGW.
	RouteDenied
	// RouteMetadata identifies an authenticated but unmetered metadata route.
	RouteMetadata
)

// Classify returns the governance policy for an HTTP method and path.
func Classify(method string, path string) RouteClass {
	if (method == http.MethodGet || method == http.MethodHead) && path == "/healthz" {
		return RoutePublic
	}
	if deniedPath(path) {
		return RouteDenied
	}
	if metadataRoute(method, path) {
		return RouteMetadata
	}
	return RouteGeneration
}

// deniedPath reports whether a path belongs to an SDK surface LLMGW never exposes.
func deniedPath(path string) bool {
	if path == "/" || path == "/management.html" {
		return true
	}
	for _, prefix := range []string{"/v0/management", "/v0/resource/plugins"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	for _, callback := range []string{
		"/anthropic/callback",
		"/codex/callback",
		"/antigravity/callback",
	} {
		if path == callback || strings.HasPrefix(path, callback+"/") {
			return true
		}
	}
	return false
}

// metadataRoute reports whether a route reads metadata or counts input tokens.
func metadataRoute(method string, path string) bool {
	if method == http.MethodGet {
		return path == "/v1/models" ||
			path == "/v1beta/models" ||
			strings.HasPrefix(path, "/v1beta/models/")
	}
	if method != http.MethodPost {
		return false
	}
	return path == "/v1/messages/count_tokens" ||
		(strings.HasPrefix(path, "/v1beta/models/") && strings.HasSuffix(path, ":countTokens"))
}
