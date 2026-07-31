package cliproxy

import (
	"context"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// alertUsagePlugin observes upstream attempts for operator alerting.
//
// It is a value type so a plugin built without a tracker is still a usable
// non-nil interface value: the service registers every extra plugin behind a
// wrapper that calls it unconditionally, and a typed nil pointer would pass
// that check and then be dereferenced.
type alertUsagePlugin struct {
	tracker *alert.Tracker // tracker receives one credential observation per record.
}

// NewAlertUsagePlugin builds the SDK usage observer feeding credential alerts.
//
// A nil tracker disables observation without disabling the plugin, which is how
// the disabled configuration is expressed with no branch at the call site.
func NewAlertUsagePlugin(tracker *alert.Tracker) sdkusage.Plugin {
	return alertUsagePlugin{tracker: tracker}
}

// HandleUsage maps one SDK attempt to one credential observation.
//
// It persists nothing and never touches the usage bridge: accounting is the
// durable plugin's job, and the LLMGW control records are filtered out before
// this observer sees them.
func (p alertUsagePlugin) HandleUsage(_ context.Context, record sdkusage.Record) {
	p.tracker.ObserveAttempt(
		record.Provider,
		record.AuthID,
		record.Model,
		record.Failed,
		record.Fail.StatusCode,
	)
}
