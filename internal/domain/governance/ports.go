package governance

import (
	"context"
	"time"
)

// KeyRepository persists and resolves project client keys.
type KeyRepository interface {
	// CreateKey persists a new project key.
	CreateKey(context.Context, string, string, string, []byte, *time.Time) (ClientKey, error)
	// KeyByPublicID resolves a client key by its non-secret identifier.
	KeyByPublicID(context.Context, string) (ClientKey, error)
	// RotateKey revokes a key and creates its replacement.
	RotateKey(context.Context, int64, string, []byte, time.Time, time.Duration) (ClientKey, error)
	// ListKeys returns every key belonging to a named project.
	ListKeys(context.Context, string) ([]ClientKey, error)
	// MarkKeyUsed records a key's most recent use.
	MarkKeyUsed(context.Context, int64, time.Time) error
	// RevokeKey records a key's revocation time.
	RevokeKey(context.Context, int64, time.Time) error
	// ExpireKey records a key's expiry time.
	ExpireKey(context.Context, int64, time.Time) error
}

// RequestRepository persists request admission and completion.
type RequestRepository interface {
	// AdmitGeneration atomically evaluates budgets and records a generation request.
	AdmitGeneration(context.Context, RequestEvent, time.Time) (Admission, error)
	// RecordMetadata records a non-generation request.
	RecordMetadata(context.Context, RequestEvent) error
	// CompleteRequest records a request's downstream completion.
	CompleteRequest(context.Context, string, int, time.Time) error
}

// BudgetRepository manages project-wide budget limits.
type BudgetRepository interface {
	// SetBudget creates or replaces one project budget.
	SetBudget(context.Context, string, Dimension, Window, float64, Action) (BudgetLimit, error)
	// ListBudgets returns every budget belonging to a named project.
	ListBudgets(context.Context, string) ([]BudgetLimit, error)
	// DeleteBudget removes a budget by database identifier.
	DeleteBudget(context.Context, int64) error
}

// UsageRepository persists, reconciles, prices, and reports usage attempts.
type UsageRepository interface {
	// PriceRuleFor resolves pricing effective for a provider, model, tier, and time.
	PriceRuleFor(context.Context, string, string, string, time.Time) (PriceRule, bool, error)
	// RecordAttempt persists one normalized upstream usage attempt.
	RecordAttempt(context.Context, UsageAttempt) error
	// RecoverInterrupted marks requests interrupted by process termination.
	RecoverInterrupted(context.Context, time.Time) (int64, error)
	// ReconcileAccounting resolves pending accounting after configured grace periods.
	ReconcileAccounting(context.Context, time.Time, time.Duration, time.Duration) (ReconcileResult, error)
	// ResolveUnknownAsZero explicitly resolves one unknown request as zero usage.
	ResolveUnknownAsZero(context.Context, string, time.Time) error
	// PruneCompletedRequests deletes completed requests older than the retention period.
	PruneCompletedRequests(context.Context, time.Duration) (int64, error)
	// QueryUsage returns grouped usage for a project and time boundary.
	QueryUsage(context.Context, UsageQuery) ([]UsageSummary, error)
}
