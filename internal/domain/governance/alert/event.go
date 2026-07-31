package alert

import "time"

// Kind identifies one notification-worthy state change.
type Kind string

const (
	// KindCredentialUnauthorized reports a credential rejected with 401 or 403.
	KindCredentialUnauthorized Kind = "credential_unauthorized"
	// KindCredentialRateLimited reports a credential rejected with 429.
	KindCredentialRateLimited Kind = "credential_rate_limited"
	// KindCredentialFailing reports a credential failing upstream.
	KindCredentialFailing Kind = "credential_failing"
	// KindCredentialRecovered reports the first success after a degraded state.
	KindCredentialRecovered Kind = "credential_recovered"
	// KindGenerationFailures reports consecutive failing generations.
	KindGenerationFailures Kind = "generation_failures"
	// KindGenerationRecovered reports the first generation served afterwards.
	KindGenerationRecovered Kind = "generation_recovered"
	// KindProjectKeyExpiring reports a project key close to its expiry.
	KindProjectKeyExpiring Kind = "project_key_expiring"
	// KindProjectKeyExpired reports a project key past its expiry.
	KindProjectKeyExpired Kind = "project_key_expired"
	// KindBudgetBlocked reports a budget denying admission.
	KindBudgetBlocked Kind = "budget_blocked"
	// KindBudgetWarning reports a breached non-blocking budget.
	KindBudgetWarning Kind = "budget_warning"
	// KindBudgetCleared reports a budget breach that no longer applies.
	KindBudgetCleared Kind = "budget_cleared"
	// KindGatewayStarted reports the service about to run.
	KindGatewayStarted Kind = "gateway_started"
	// KindGatewayStopping reports the service returning.
	KindGatewayStopping Kind = "gateway_stopping"
	// KindDatabaseUnavailable reports a genuine PostgreSQL failure.
	KindDatabaseUnavailable Kind = "database_unavailable"
	// KindDatabaseRestored reports the first success after an outage.
	KindDatabaseRestored Kind = "database_restored"
	// KindUsagePoisoned reports the terminal usage-correlation state.
	KindUsagePoisoned Kind = "usage_correlation_poisoned"
	// KindProjectKeyCreated reports an operator-created project key.
	KindProjectKeyCreated Kind = "project_key_created"
	// KindProjectKeyRotated reports an operator-rotated project key.
	KindProjectKeyRotated Kind = "project_key_rotated"
	// KindCredentialAdded reports an operator-added provider credential.
	KindCredentialAdded Kind = "provider_credential_added"
	// KindCredentialsImported reports an operator-run legacy import.
	KindCredentialsImported Kind = "provider_credentials_imported"
)

// Severity classifies how urgently an event needs an operator.
type Severity string

const (
	// SeverityCritical marks an event that needs an operator now.
	SeverityCritical Severity = "critical"
	// SeverityWarning marks a degradation an operator should look at.
	SeverityWarning Severity = "warning"
	// SeverityInfo marks an event that is recorded rather than acted on.
	SeverityInfo Severity = "info"
)

// Field is one labelled value rendered beside an event.
type Field struct {
	Name  string // Name labels the value.
	Value string // Value is the already-safe rendered value.
}

// Event is one notification-worthy state change.
type Event struct {
	Kind     Kind      // Kind identifies the change.
	Severity Severity  // Severity is derived from Kind.
	Summary  string    // Summary is the one-line human description.
	Fields   []Field   // Fields carry the identifying context.
	At       time.Time // At is the UTC observation time.
}

// Notifier delivers events without blocking its caller.
//
// A Tracker holds its mutex across Notify, so an implementation must neither
// block nor call back into the tracker that invoked it.
type Notifier interface {
	// Notify reports whether the event was accepted for delivery.
	Notify(Event) bool
}

// CredentialLabel is the operator-facing identity of one provider credential.
type CredentialLabel struct {
	Provider string // Provider names the credential's provider.
	Label    string // Label is the operator-facing account name.
}

// severityOf maps every kind to its fixed severity. Absent kinds are info.
var severityOf = map[Kind]Severity{
	KindCredentialUnauthorized: SeverityCritical,
	KindGenerationFailures:     SeverityCritical,
	KindBudgetBlocked:          SeverityCritical,
	KindDatabaseUnavailable:    SeverityCritical,
	KindUsagePoisoned:          SeverityCritical,

	KindCredentialRateLimited: SeverityWarning,
	KindCredentialFailing:     SeverityWarning,
	KindProjectKeyExpiring:    SeverityWarning,
	KindProjectKeyExpired:     SeverityWarning,
	KindBudgetWarning:         SeverityWarning,
}

// titleOf maps every kind to its human title, also used as the event summary.
var titleOf = map[Kind]string{
	KindCredentialUnauthorized: "Provider credential unauthorized",
	KindCredentialRateLimited:  "Provider credential rate limited",
	KindCredentialFailing:      "Provider credential failing",
	KindCredentialRecovered:    "Provider credential recovered",
	KindGenerationFailures:     "Generation failures",
	KindGenerationRecovered:    "Generation recovered",
	KindProjectKeyExpiring:     "Project key expiring",
	KindProjectKeyExpired:      "Project key expired",
	KindBudgetBlocked:          "Budget blocked",
	KindBudgetWarning:          "Budget warning",
	KindBudgetCleared:          "Budget cleared",
	KindGatewayStarted:         "Gateway started",
	KindGatewayStopping:        "Gateway stopping",
	KindDatabaseUnavailable:    "Database unavailable",
	KindDatabaseRestored:       "Database restored",
	KindUsagePoisoned:          "Usage correlation poisoned",
	KindProjectKeyCreated:      "Project key created",
	KindProjectKeyRotated:      "Project key rotated",
	KindCredentialAdded:        "Provider credential added",
	KindCredentialsImported:    "Provider credentials imported",
}

// severity returns the kind's fixed severity, so a new kind can never panic.
func (k Kind) severity() Severity {
	if value, found := severityOf[k]; found {
		return value
	}
	return SeverityInfo
}

// Title returns the kind's human title, falling back to its identifier.
//
// It is exported because the Discord renderer needs it: a second copy of the
// table on the adapter side would drift from this one.
func (k Kind) Title() string {
	if value, found := titleOf[k]; found {
		return value
	}
	return string(k)
}

// severityRank orders severities so an escalation can be detected.
func severityRank(severity Severity) int {
	switch severity {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	default:
		return 1
	}
}
