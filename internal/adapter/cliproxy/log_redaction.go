package cliproxy

import (
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

const (
	// upstreamFailureMessageMarker identifies the embedded SDK's upstream
	// failure warning. Its trailing error field carries the provider response
	// body verbatim, and that body is caller-influenced text: a provider is
	// free to quote the request back inside its own error message. The SDK
	// emits the warning in two shapes — bare, and prefixed with the upstream
	// status and duration — so the marker is matched anywhere in the message,
	// not just at its start.
	upstreamFailureMessageMarker = "upstream execution failed: "
	// upstreamFailureErrorMarker opens the provider body inside that warning.
	upstreamFailureErrorMarker = " err="
	// redactedUpstreamError replaces the provider body. The failure itself
	// stays visible, and the upstream status the body would document is
	// recorded with the usage attempt, so no diagnosis depends on the log.
	redactedUpstreamError = " err=<redacted>"
)

// upstreamFailureRedactionOnce guards the sole process-wide hook installation.
var upstreamFailureRedactionOnce sync.Once

// armUpstreamFailureRedaction installs the redaction hook once per process.
func armUpstreamFailureRedaction() {
	upstreamFailureRedactionOnce.Do(func() {
		log.AddHook(upstreamFailureRedaction{})
	})
}

// upstreamFailureRedaction keeps provider error bodies out of gateway logs.
type upstreamFailureRedaction struct{}

// Levels restricts the hook to the level the SDK reports upstream failures at.
func (upstreamFailureRedaction) Levels() []log.Level {
	return []log.Level{log.WarnLevel}
}

// Fire replaces the provider error body with a fixed placeholder.
func (upstreamFailureRedaction) Fire(entry *log.Entry) error {
	if entry == nil || !strings.Contains(entry.Message, upstreamFailureMessageMarker) {
		return nil
	}

	marker := strings.Index(entry.Message, upstreamFailureErrorMarker)
	if marker < 0 {
		return nil
	}

	entry.Message = entry.Message[:marker] + redactedUpstreamError
	return nil
}
