package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The xAI executor reads its base URL from the credential file, which is what lets
// file-backed credentials of a real provider be pointed at the stub upstream.
const (
	priorityModel       = "grok-4.7"
	priorityMainFile    = "xai-priority-main.json"
	prioritySpareAFile  = "xai-priority-spare-a.json"
	prioritySpareBFile  = "xai-priority-spare-b.json"
	priorityMainToken   = "upstream-xai-priority-main"
	prioritySpareAToken = "upstream-xai-priority-spare-a"
	prioritySpareBToken = "upstream-xai-priority-spare-b"
)

// priorityBaseURL spells the stub's address with the loopback name rather than
// the IP. The SDK's xAI executor logs its resolved base URL by design, and the
// harness audit treats the stub's exact URL as a secret no log may carry.
func priorityBaseURL(upstreamURL string) string {
	return strings.Replace(upstreamURL, "127.0.0.1", "localhost", 1)
}

// writePriorityAuthFiles writes three file-backed xAI credentials into the auth
// directory before the gateway starts. Each carries a distinct token so the stub
// can tell which account served a request; the main one holds the priority tier.
func writePriorityAuthFiles(authDir, upstreamURL string) error {
	files := []struct {
		name     string
		token    string
		priority int
	}{
		{priorityMainFile, priorityMainToken, 10},
		{prioritySpareAFile, prioritySpareAToken, 0},
		{prioritySpareBFile, prioritySpareBToken, 0},
	}
	for _, file := range files {
		body := map[string]any{
			"type":         "xai",
			"email":        file.name,
			"access_token": file.token,
			"base_url":     priorityBaseURL(upstreamURL),
		}
		if file.priority != 0 {
			body["priority"] = file.priority
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode priority auth file:\n%w", err)
		}
		if err := os.WriteFile(filepath.Join(authDir, file.name), raw, 0o600); err != nil {
			return fmt.Errorf("write priority auth file:\n%w", err)
		}
	}
	return nil
}

// priorityUsageResponse returns a successful xAI Responses stream.
func priorityUsageResponse() StubResponse {
	return codexUsageResponse(priorityModel)
}

// priorityRateLimit is the upstream 429 that cools one account down.
func priorityRateLimit() StubResponse {
	return StubResponse{
		Status:  http.StatusTooManyRequests,
		Headers: http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"3600"}},
		Body:    `{"error":{"message":"priority fixture","type":"rate_limit_error"}}`,
	}
}
