package claudemax

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"unicode/utf16"
)

const (
	// billingSalt is the fixed salt clewdr mixes into the billing-header version hash.
	billingSalt = "59cf53e54c78"

	// billingEntrypoint is the entrypoint reported in the billing header (the CLI surface).
	billingEntrypoint = "cli"
)

// sampleIndices are the UTF-16 code-unit positions of the first user message sampled into
// the billing-header hash, matching clewdr's claude_code_billing_header.
var sampleIndices = []int{4, 7, 20}

// spoof produces the Claude Code request markers (billing-header system block + HTTP
// headers) for the configured client version.
type spoof struct {
	version string // version is the spoofed Claude Code client version (e.g. "2.1.212").
}

// billingHeader builds the billing-header system block from the first user message text.
// It replicates clewdr's claude_code_billing_header: the hash is the first 3 hex digits of
// sha256(salt + sampled code units at indices 4,7,20 + version).
func (s spoof) billingHeader(firstUserText string) string {
	var sampled string
	for _, idx := range sampleIndices {
		sampled += sampleCodeUnit(firstUserText, idx)
	}

	digest := sha256.Sum256([]byte(billingSalt + sampled + s.version))
	hash3 := hex.EncodeToString(digest[:])[:3]

	return fmt.Sprintf(
		"x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=00000;",
		s.version, hash3, billingEntrypoint,
	)
}

// sampleCodeUnit returns the UTF-16 code unit at idx as a string, or "0" when out of range.
// It mirrors clewdr's sample_js_code_unit, which samples JavaScript string code units.
func sampleCodeUnit(text string, idx int) string {
	units := utf16.Encode([]rune(text))
	if idx >= len(units) {
		return "0"
	}
	return string(utf16.Decode([]uint16{units[idx]}))
}

// decorate sets the Claude Code spoof headers on an upstream request: the OAuth bearer,
// the spoofed user agent, and the fixed beta/version markers.
func (s spoof) decorate(req *http.Request, accessToken string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "claude-code/"+s.version)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Content-Type", "application/json")
}
