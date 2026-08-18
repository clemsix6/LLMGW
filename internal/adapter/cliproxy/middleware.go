package cliproxy

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
	"github.com/clemsix6/LLMGW/internal/domain/projectkey"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// completionTimeout bounds detached request-finalization work.
	completionTimeout = 5 * time.Second
	// completionAttempts bounds repository attempts for request finalization.
	completionAttempts = 3
)

// KeyAuthenticator validates a project credential without exposing key storage.
type KeyAuthenticator interface {
	// Authenticate returns the project identity for a valid raw credential.
	Authenticate(context.Context, string) (governance.KeyIdentity, error)
}

// Middleware coordinates route policy, project authentication, request
// lifecycle, and bounded SDK usage groups.
type Middleware struct {
	keys     KeyAuthenticator             // keys authenticates project credentials.
	requests governance.RequestRepository // requests persists admission and completion.
	now      func() time.Time             // now supplies lifecycle timestamps.
	bridge   *UsageBridge                 // bridge owns generation permits and FIFO barriers.
	ready    <-chan struct{}              // ready closes when the SDK can actually serve.
	tracker  *alert.Tracker               // tracker observes admissions, generations and database health.
}

// errorEnvelope is the stable outer JSON error shape.
type errorEnvelope struct {
	Error errorDetail `json:"error"` // Error contains the safe client-facing detail.
}

// errorDetail is a safe client-facing error without internal causes.
type errorDetail struct {
	Type string `json:"type"` // Type is the stable machine-readable classifier.
}

// budgetErrorEnvelope is the stable budget-block JSON shape.
type budgetErrorEnvelope struct {
	Error budgetErrorDetail `json:"error"` // Error contains the breached budget fields.
}

// budgetErrorDetail describes only the rule needed to understand a block.
type budgetErrorDetail struct {
	Type      string               `json:"type"`      // Type is always budget_exceeded.
	Dimension governance.Dimension `json:"dimension"` // Dimension is the blocked quantity.
	Window    governance.Window    `json:"window"`    // Window is the blocked rolling window.
	ResetAt   string               `json:"reset_at"`  // ResetAt is the UTC reset timestamp.
}

// NewMiddleware builds the global LLMGW governance middleware for the SDK.
//
// A nil tracker disables alert observation: every Tracker method returns
// immediately on a nil receiver, so no observation point needs a guard.
func NewMiddleware(
	keys KeyAuthenticator,
	requests governance.RequestRepository,
	now func() time.Time,
	bridge *UsageBridge,
	tracker *alert.Tracker,
) *Middleware {
	return &Middleware{
		keys:     keys,
		requests: requests,
		now:      now,
		bridge:   bridge,
		tracker:  tracker,
	}
}

// Handler exposes the concrete bridge-bound middleware to Gin.
func (m *Middleware) Handler() gin.HandlerFunc {
	return m.handle
}

// serveWhenReady refuses traffic until the SDK finishes startup. The SDK opens
// its listener before it can serve, so without this the first requests after a
// restart fail upstream while the health route already reports success.
func (m *Middleware) serveWhenReady(ready <-chan struct{}) {
	m.ready = ready
}

// serving reports whether SDK startup has completed. A middleware built without
// a readiness signal always serves.
func (m *Middleware) serving() bool {
	if m.ready == nil {
		return true
	}
	select {
	case <-m.ready:
		return true
	default:
		return false
	}
}

// handle applies route policy before any SDK handler or access provider runs.
func (m *Middleware) handle(c *gin.Context) {
	class := Classify(c.Request.Method, c.Request.URL.Path)
	// The deny policy stays unconditional: a transient startup state must never
	// change which surfaces exist.
	if class == RouteDenied {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if !m.serving() {
		abortSafe(c, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	if class == RoutePublic {
		// A poisoned bridge can no longer admit any generation. Report it on the
		// health route so the state is observable instead of silently degraded.
		if m.bridge != nil && m.bridge.poisoned() {
			abortSafe(c, http.StatusServiceUnavailable, "service_unavailable")
			return
		}
		c.Next()
		return
	}

	keyIdentity, ok := m.authenticate(c)
	if !ok {
		return
	}
	m.admit(c, class, keyIdentity)
}

// authenticate resolves the single accepted project credential.
func (m *Middleware) authenticate(c *gin.Context) (governance.KeyIdentity, bool) {
	raw, err := credential(c.Request.Header)
	if err != nil {
		abortSafe(c, http.StatusUnauthorized, "authentication_error")
		return governance.KeyIdentity{}, false
	}
	identity, err := m.keys.Authenticate(c.Request.Context(), raw)
	if errors.Is(err, projectkey.ErrInvalidCredential) {
		abortSafe(c, http.StatusUnauthorized, "authentication_error")
		return governance.KeyIdentity{}, false
	}
	if err != nil {
		log.Print("llmgw: authenticate project key: unavailable")
		abortSafe(c, http.StatusServiceUnavailable, "service_unavailable")
		return governance.KeyIdentity{}, false
	}
	return identity, true
}

// admit records an authenticated request before passing control to the SDK.
func (m *Middleware) admit(
	c *gin.Context,
	class RouteClass,
	keyIdentity governance.KeyIdentity,
) {
	requestedAt := m.now().UTC()
	request := newRequestEvent(c, class, keyIdentity, requestedAt)
	reserved := false
	if class == RouteGeneration {
		if m.bridge == nil || !m.bridge.reserve(request.ID) {
			abortSafe(c, http.StatusServiceUnavailable, "service_unavailable")
			return
		}
		reserved = true
	}
	admission, ok := m.recordRequest(c, class, request, requestedAt)
	if !ok {
		if reserved {
			m.bridge.release(request.ID)
		}
		return
	}
	if class == RouteGeneration {
		// Before the abort: after it every observed admission is allowed, so a
		// block could never be reported.
		m.tracker.ObserveAdmission(keyIdentity.ProjectName, admission.Blocks, admission.Warnings)
		if !admission.Allowed {
			m.bridge.release(request.ID)
			m.abortBudget(c, admission)
			return
		}
	}
	if !m.rewriteRequest(c, keyIdentity, request.ID, reserved) {
		return
	}

	identity := requestIdentity(request, keyIdentity)
	m.logWarnings(identity.ProjectID, admission.Warnings)
	requestContext := WithIdentity(c.Request.Context(), identity)
	if class == RouteGeneration {
		var stopCancellationWatch func()
		requestContext, stopCancellationWatch = withUsageCancellationMarker(
			requestContext,
			m.bridge,
			identity.RequestID,
		)
		c.Request = c.Request.WithContext(requestContext)
		// Register the barrier defer before completion so LIFO ordering publishes
		// only after detached completion has returned. A downstream panic still
		// runs both defers and then propagates unchanged.
		defer func() {
			stopCancellationWatch()
			m.bridge.publishBarrier(
				identity.RequestID,
				c.Request.Context().Err() != nil,
			)
			// Read inside the closure: a deferred call evaluates the status before
			// c.Next runs, observing every generation as the writer's default 200.
			m.tracker.ObserveGeneration(c.Writer.Status())
		}()
	} else {
		c.Request = c.Request.WithContext(requestContext)
	}
	defer m.complete(c, identity)
	if finalizeResponse := installToolPrefixWriter(c, keyIdentity); finalizeResponse != nil {
		// Registered after complete so LIFO runs it first: the rewritten response
		// must be fully on the wire before completion records the status it ended on.
		defer finalizeResponse()
	}
	if finalizeGuard := installMarkupGuard(c, keyIdentity, identity.RequestID); finalizeGuard != nil {
		// Registered last so LIFO runs it before every other finalization: the
		// screen decides the status the layers beneath it commit and record.
		defer finalizeGuard()
	}
	c.Next()
}

// newRequestEvent builds the persistence value without reading the request body.
func newRequestEvent(
	c *gin.Context,
	class RouteClass,
	identity governance.KeyIdentity,
	requestedAt time.Time,
) governance.RequestEvent {
	operation := governance.OperationGeneration
	if class == RouteMetadata {
		operation = governance.OperationMetadata
	}
	return governance.RequestEvent{
		ID:          uuid.NewString(),
		ProjectID:   identity.ProjectID,
		ClientKeyID: identity.ClientKeyID,
		Operation:   operation,
		RequestedAt: requestedAt,
		Method:      c.Request.Method,
		Path:        c.Request.URL.Path,
	}
}

// recordRequest delegates to the operation-specific atomic repository method.
func (m *Middleware) recordRequest(
	c *gin.Context,
	class RouteClass,
	request governance.RequestEvent,
	requestedAt time.Time,
) (governance.Admission, bool) {
	if class == RouteMetadata {
		if err := m.requests.RecordMetadata(c.Request.Context(), request); err != nil {
			m.observeRepositoryFailure(c)
			log.Printf("llmgw: record metadata request (project=%d): unavailable", request.ProjectID)
			abortSafe(c, http.StatusServiceUnavailable, "service_unavailable")
			return governance.Admission{}, false
		}
		m.tracker.ObserveDatabase(true)
		return governance.Admission{Allowed: true, Request: request}, true
	}

	admission, err := m.requests.AdmitGeneration(c.Request.Context(), request, requestedAt)
	if err != nil {
		m.observeRepositoryFailure(c)
		log.Printf("llmgw: admit generation (project=%d): unavailable", request.ProjectID)
		abortSafe(c, http.StatusServiceUnavailable, "service_unavailable")
		return governance.Admission{}, false
	}
	m.tracker.ObserveDatabase(true)
	return admission, true
}

// observeRepositoryFailure reports a repository error as a database outage only
// while the caller is still there: a client that walked away aborts its own
// query, which says nothing about PostgreSQL's health.
func (m *Middleware) observeRepositoryFailure(c *gin.Context) {
	if c.Request.Context().Err() != nil {
		return
	}
	m.tracker.ObserveDatabase(false)
}

// requestIdentity converts authenticated and admitted values into SDK context identity.
func requestIdentity(
	request governance.RequestEvent,
	key governance.KeyIdentity,
) RequestIdentity {
	return RequestIdentity{
		RequestID:   request.ID,
		ProjectID:   key.ProjectID,
		ClientKeyID: key.ClientKeyID,
		KeyPublicID: key.PublicID,
	}
}

// abortBudget returns only the first deterministic blocking breach.
func (m *Middleware) abortBudget(c *gin.Context, admission governance.Admission) {
	if len(admission.Blocks) == 0 {
		abortSafe(c, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	block := admission.Blocks[0]
	c.AbortWithStatusJSON(http.StatusPaymentRequired, budgetErrorEnvelope{
		Error: budgetErrorDetail{
			Type:      "budget_exceeded",
			Dimension: block.Limit.Dimension,
			Window:    block.Limit.Window,
			ResetAt:   block.ResetAt.UTC().Format(time.RFC3339),
		},
	})
}

// logWarnings records non-blocking rules without any key material.
func (m *Middleware) logWarnings(projectID int64, warnings []governance.BudgetBreach) {
	for _, warning := range warnings {
		log.Printf(
			"llmgw: budget warning (project=%d dimension=%s window=%s)",
			projectID,
			warning.Limit.Dimension,
			warning.Limit.Window,
		)
	}
}

// complete persists the final downstream status with detached bounded retries.
func (m *Middleware) complete(c *gin.Context, identity RequestIdentity) {
	base := context.WithoutCancel(c.Request.Context())
	ctx, cancel := context.WithTimeout(base, completionTimeout)
	defer cancel()

	completedAt := m.now().UTC()
	for attempt := 0; attempt < completionAttempts; attempt++ {
		err := m.requests.CompleteRequest(
			ctx,
			identity.RequestID,
			c.Writer.Status(),
			completedAt,
		)
		if err == nil {
			m.tracker.ObserveDatabase(true)
			return
		}
		if attempt+1 < completionAttempts && waitCompletionRetry(ctx, attempt) {
			continue
		}
		break
	}
	// Reported once the retries are exhausted, and unconditionally: this work
	// runs under context.WithoutCancel, so a deadline here is genuine slowness.
	// Reporting per attempt would page for a blip the retry loop absorbs.
	m.tracker.ObserveDatabase(false)
	log.Printf(
		"llmgw: complete request (project=%d request=%s): unavailable",
		identity.ProjectID,
		identity.RequestID,
	)
}

// waitCompletionRetry waits for the next bounded 50ms or 100ms retry.
func waitCompletionRetry(ctx context.Context, attempt int) bool {
	delay := 50 * time.Millisecond * time.Duration(attempt+1)
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// abortSafe writes an error response containing no supplied or internal values.
func abortSafe(c *gin.Context, status int, errorType string) {
	c.AbortWithStatusJSON(status, errorEnvelope{Error: errorDetail{Type: errorType}})
}
