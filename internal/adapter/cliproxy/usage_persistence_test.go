package cliproxy

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/google/uuid"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestUsagePluginRetriesWithStableAttempt verifies bounded detached persistence.
func TestUsagePluginRetriesWithStableAttempt(t *testing.T) {
	bridge := fixedUsageBridge(t)
	requestID := uuid.NewString()
	identity := RequestIdentity{RequestID: requestID, KeyPublicID: "MDEyMzQ1Njc4OWFi"}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	callback, cancel := usageCallbackContext(t, identity)
	cancel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	transient := errors.New("transient")
	var attempts []governance.UsageAttempt
	var deadlines []time.Duration
	var recordedAt []time.Time
	repository := &usageRepositoryStub{
		priceRuleFor: func(
			ctx context.Context,
			provider string,
			model string,
			tier string,
			requestedAt time.Time,
		) (governance.PriceRule, bool, error) {
			if ctx.Err() != nil {
				t.Fatalf("price context retained request cancellation: %v", ctx.Err())
			}
			if ctx.Value("gin") != nil {
				t.Fatal("usage persistence context retained recycled Gin state")
			}
			if provider != "openai-compatibility" || model != "upstream-model" ||
				tier != "default" || !requestedAt.Equal(now.Add(-time.Second)) {
				t.Fatalf("price lookup = (%q, %q, %q, %v)", provider, model, tier, requestedAt)
			}
			return fullyPricedRule(), true, nil
		},
		recordAttempt: func(ctx context.Context, attempt governance.UsageAttempt) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("usage persistence context has no deadline")
			}
			deadlines = append(deadlines, time.Until(deadline))
			attempts = append(attempts, attempt)
			recordedAt = append(recordedAt, time.Now())
			if len(attempts) < 3 {
				return transient
			}
			return nil
		},
	}
	plugin := NewUsagePlugin(repository, bridge, func(err error) bool {
		return errors.Is(err, transient)
	})
	record := sdkusage.Record{
		Provider:            "openai-compatibility",
		ExecutorType:        "OpenAICompatExecutor",
		Model:               "upstream-model",
		Alias:               "test-model",
		APIKey:              principalFor(t, bridge, identity),
		ServiceTier:         "auto",
		ResponseServiceTier: "default",
		RequestedAt:         now.Add(-time.Second),
		Detail: sdkusage.Detail{
			InputTokens:  10,
			OutputTokens: 4,
			TotalTokens:  14,
		},
	}

	plugin.HandleUsage(callback, record)

	if len(attempts) != 3 {
		t.Fatalf("record attempts = %d, want 3", len(attempts))
	}
	for index, attempt := range attempts {
		if attempt.ID == "" || attempt.ID != attempts[0].ID {
			t.Fatalf("attempt %d ID = %q, want stable %q", index, attempt.ID, attempts[0].ID)
		}
		if attempt.RequestID != requestID {
			t.Fatalf("attempt request ID = %q, want %q", attempt.RequestID, requestID)
		}
		if !attempt.CreatedAt.Equal(record.RequestedAt) ||
			attempt.CreatedAt.Location() != time.UTC {
			t.Fatalf(
				"attempt creation time = %v (%v), want SDK request time %v UTC",
				attempt.CreatedAt,
				attempt.CreatedAt.Location(),
				record.RequestedAt,
			)
		}
	}
	firstDelay := recordedAt[1].Sub(recordedAt[0])
	secondDelay := recordedAt[2].Sub(recordedAt[1])
	if firstDelay < 40*time.Millisecond || secondDelay < 90*time.Millisecond {
		t.Fatalf("retry delays = (%v, %v), want at least (40ms, 90ms)", firstDelay, secondDelay)
	}
	if attempts[2].CostUSD == nil || attempts[2].PricingState != governance.PricingPriced {
		t.Fatalf("priced attempt = %#v", attempts[2])
	}
	for _, remaining := range deadlines {
		if remaining <= 4*time.Second || remaining > 5*time.Second {
			t.Fatalf("usage deadline remaining = %v, want (4s, 5s]", remaining)
		}
	}
}

// TestUsagePluginDoesNotRetryPermanentErrors verifies permanent repository
// failures receive one attempt and no retry delay.
func TestUsagePluginDoesNotRetryPermanentErrors(t *testing.T) {
	bridge := fixedUsageBridge(t)
	requestID := uuid.NewString()
	identity := RequestIdentity{RequestID: requestID, KeyPublicID: "MDEyMzQ1Njc4OWFi"}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	callback, _ := usageCallbackContext(t, identity)
	permanent := errors.New("permanent")
	var calls int
	repository := &usageRepositoryStub{
		priceRuleFor: func(
			context.Context,
			string,
			string,
			string,
			time.Time,
		) (governance.PriceRule, bool, error) {
			return governance.PriceRule{}, false, nil
		},
		recordAttempt: func(context.Context, governance.UsageAttempt) error {
			calls++
			return permanent
		},
	}

	NewUsagePlugin(repository, bridge, func(error) bool { return false }).HandleUsage(
		callback,
		usageRecordForTest(principalFor(t, bridge, identity)),
	)

	if calls != 1 {
		t.Fatalf("permanent repository calls = %d, want 1", calls)
	}
}

// TestUsagePluginRejectsZeroRequestedAt catches silently substituting callback
// time or the parent request time when the pinned SDK record omits its required
// attempt timestamp.
func TestUsagePluginRejectsZeroRequestedAt(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	repository := &usageRepositoryStub{
		priceRuleFor: func(
			context.Context,
			string,
			string,
			string,
			time.Time,
		) (governance.PriceRule, bool, error) {
			t.Fatal("zero RequestedAt reached price lookup")
			return governance.PriceRule{}, false, nil
		},
		recordAttempt: func(context.Context, governance.UsageAttempt) error {
			t.Fatal("zero RequestedAt reached persistence")
			return nil
		},
	}

	NewUsagePlugin(repository, bridge, nil).HandleUsage(
		context.Background(),
		sdkusage.Record{APIKey: principalFor(t, bridge, identity)},
	)

	if bridge.outstanding() != 1 || bridge.reserve(uuid.NewString()) {
		t.Fatal("invalid timestamp returned fail-closed usage capacity")
	}
}

// TestUsagePluginPersistsUnknownPricing verifies missing rules do not lose tokens.
func TestUsagePluginPersistsUnknownPricing(t *testing.T) {
	bridge := fixedUsageBridge(t)
	requestID := uuid.NewString()
	identity := RequestIdentity{RequestID: requestID, KeyPublicID: "MDEyMzQ1Njc4OWFi"}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	callback, _ := usageCallbackContext(t, identity)
	var got governance.UsageAttempt
	repository := &usageRepositoryStub{
		priceRuleFor: func(
			context.Context,
			string,
			string,
			string,
			time.Time,
		) (governance.PriceRule, bool, error) {
			return governance.PriceRule{}, false, nil
		},
		recordAttempt: func(_ context.Context, attempt governance.UsageAttempt) error {
			got = attempt
			return nil
		},
	}
	record := sdkusage.Record{
		Provider:    "openai-compatibility",
		Model:       "unpriced-model",
		Alias:       "unpriced-alias",
		APIKey:      principalFor(t, bridge, identity),
		RequestedAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		Detail:      sdkusage.Detail{InputTokens: 2, OutputTokens: 1, TotalTokens: 3},
	}

	NewUsagePlugin(repository, bridge, nil).HandleUsage(callback, record)

	if got.RequestID != requestID || got.ClientKeyPublicID != identity.KeyPublicID ||
		got.CostUSD != nil ||
		got.PricingState != governance.PricingUnknown || got.Tokens.Total != 3 {
		t.Fatalf("unknown-priced attempt = %#v", got)
	}
}

// TestUsagePluginPanicRecoveryIsValueFree verifies callback panics cannot leak payloads.
func TestUsagePluginPanicRecoveryIsValueFree(t *testing.T) {
	const secret = "secret-record-value"
	bridge := fixedUsageBridge(t)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	callback, _ := usageCallbackContext(t, identity)
	repository := &usageRepositoryStub{
		priceRuleFor: func(
			context.Context,
			string,
			string,
			string,
			time.Time,
		) (governance.PriceRule, bool, error) {
			panic(secret)
		},
	}
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })

	NewUsagePlugin(repository, bridge, nil).HandleUsage(
		callback,
		sdkusage.Record{
			APIKey:      principalFor(t, bridge, identity),
			RequestedAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
			Source:      secret,
		},
	)
	barrier, _ := bridge.barrier(identity.RequestID)
	NewUsagePlugin(repository, bridge, nil).HandleUsage(
		context.Background(),
		sdkusage.Record{APIKey: barrier},
	)

	if !strings.Contains(logs.String(), "usage plugin panic recovered") {
		t.Fatalf("panic recovery log = %q", logs.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("panic recovery log contains record or panic value")
	}
	if bridge.outstanding() != 1 {
		t.Fatal("plugin panic returned generation capacity")
	}
}

func TestUsagePluginReleasesOnlyAfterFIFOBarrier(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	var persisted int
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(governance.UsageAttempt) { persisted++ }),
		bridge,
		nil,
	)

	plugin.HandleUsage(
		context.Background(),
		usageRecordForTest(principalFor(t, bridge, identity)),
	)
	if persisted != 1 || bridge.outstanding() != 1 {
		t.Fatalf("before barrier persisted/outstanding = %d/%d, want 1/1",
			persisted, bridge.outstanding())
	}
	barrier, ok := bridge.barrier(identity.RequestID)
	if !ok {
		t.Fatal("barrier rejected identity")
	}
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: barrier})
	if bridge.outstanding() != 0 {
		t.Fatalf("after barrier outstanding = %d, want 0", bridge.outstanding())
	}
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: barrier})
	if bridge.outstanding() != 0 || persisted != 1 {
		t.Fatal("duplicate barrier changed state or persisted a synthetic attempt")
	}
}

func TestUsagePluginCanceledBarrierWaitsForDurableLateTerminal(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	var persisted []bool
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(attempt governance.UsageAttempt) {
			persisted = append(persisted, attempt.Failed)
		}),
		bridge,
		nil,
	)
	principal := principalFor(t, bridge, identity)

	// Bootstrap failovers finish before the active producer returns a stream.
	plugin.HandleUsage(context.Background(), failedUsageRecordForTest(principal))
	barrier, ok := bridge.barrierFor(identity.RequestID, true)
	if !ok {
		t.Fatal("canceled barrier rejected identity")
	}
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: barrier})
	if bridge.outstanding() != 1 {
		t.Fatal("prior failed attempt was confused with canceled stream terminal")
	}

	// The first post-barrier record can only belong to the one active producer.
	plugin.HandleUsage(context.Background(), failedUsageRecordForTest(principal))
	if bridge.outstanding() != 0 || len(persisted) != 2 ||
		!persisted[0] || !persisted[1] {
		t.Fatalf("late terminal state = outstanding:%d persisted:%v, want 0/[true true]",
			bridge.outstanding(), persisted)
	}
}

// TestUsagePluginCanceledLateTerminalCompletesTheGroup proves that a canceled
// group which crossed its barrier is completed and its permit returned by the
// first durable record the producer still emits.
func TestUsagePluginCanceledLateTerminalCompletesTheGroup(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	var persisted int
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(governance.UsageAttempt) {
			persisted++
		}),
		bridge,
		nil,
	)
	principal := principalFor(t, bridge, identity)
	barrier, _ := bridge.barrierFor(identity.RequestID, true)
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: barrier})

	if bridge.outstanding() != 1 || bridge.poisoned() {
		t.Fatalf("after canceled barrier = outstanding:%d poisoned:%t, want 1/false",
			bridge.outstanding(), bridge.poisoned())
	}
	plugin.HandleUsage(context.Background(), usageRecordForTest(principal))

	if persisted != 1 || bridge.poisoned() || bridge.outstanding() != 0 ||
		!bridge.reserve(uuid.NewString()) {
		t.Fatalf("late group = persisted:%d poisoned:%t outstanding:%d, want 1/false/0",
			persisted, bridge.poisoned(), bridge.outstanding())
	}
}

func TestUsageBridgeWaitDrainedWaitsForCanceledLateTerminal(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(governance.UsageAttempt) {}),
		bridge,
		nil,
	)
	barrier, _ := bridge.barrierFor(identity.RequestID, true)
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: barrier})

	drained := make(chan error, 1)
	go func() {
		drained <- bridge.waitDrained(context.Background())
	}()
	select {
	case err := <-drained:
		t.Fatalf("waitDrained returned before late terminal: %v", err)
	default:
	}

	plugin.HandleUsage(
		context.Background(),
		usageRecordForTest(principalFor(t, bridge, identity)),
	)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitDrained did not return after late terminal")
	}
}

func TestUsagePluginCanceledMarkerSeparatesPriorFailureFromActiveTerminal(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(governance.UsageAttempt) {}),
		bridge,
		nil,
	)
	principal := principalFor(t, bridge, identity)

	plugin.HandleUsage(context.Background(), failedUsageRecordForTest(principal))
	cancelMarker, ok := bridge.cancel(identity.RequestID)
	if !ok {
		t.Fatal("cancel marker rejected identity")
	}
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: cancelMarker})
	plugin.HandleUsage(context.Background(), failedUsageRecordForTest(principal))
	barrier, _ := bridge.barrierFor(identity.RequestID, true)
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: barrier})

	if bridge.outstanding() != 0 {
		t.Fatal("durable active terminal between cancel marker and barrier retained capacity")
	}
}

func TestUsagePluginCanceledBarrierAcceptsDurableSuccessAlreadyInFIFO(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(governance.UsageAttempt) {}),
		bridge,
		nil,
	)
	plugin.HandleUsage(
		context.Background(),
		usageRecordForTest(principalFor(t, bridge, identity)),
	)
	barrier, _ := bridge.barrierFor(identity.RequestID, true)
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: barrier})

	if bridge.outstanding() != 0 {
		t.Fatal("durable successful terminal before canceled barrier retained capacity")
	}
}

func TestUsagePluginCanceledLatePersistenceFailureKeepsPermit(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	permanent := errors.New("permanent")
	repository := successfulUsageRepository(func(governance.UsageAttempt) {})
	repository.recordAttempt = func(context.Context, governance.UsageAttempt) error {
		return permanent
	}
	plugin := NewUsagePlugin(repository, bridge, func(error) bool { return false })
	barrier, _ := bridge.barrierFor(identity.RequestID, true)
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: barrier})
	plugin.HandleUsage(
		context.Background(),
		usageRecordForTest(principalFor(t, bridge, identity)),
	)

	if bridge.outstanding() != 1 || bridge.reserve(uuid.NewString()) {
		t.Fatal("failed late terminal persistence returned canceled-stream capacity")
	}
}

func TestUsagePluginPersistenceFailureKeepsPermitHeld(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	identity := RequestIdentity{
		RequestID:   uuid.NewString(),
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	if !bridge.reserve(identity.RequestID) {
		t.Fatal("reserve failed")
	}
	permanent := errors.New("permanent")
	repository := successfulUsageRepository(func(governance.UsageAttempt) {})
	repository.recordAttempt = func(context.Context, governance.UsageAttempt) error {
		return permanent
	}
	plugin := NewUsagePlugin(repository, bridge, func(error) bool { return false })

	plugin.HandleUsage(
		context.Background(),
		usageRecordForTest(principalFor(t, bridge, identity)),
	)
	barrier, _ := bridge.barrier(identity.RequestID)
	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: barrier})

	if bridge.outstanding() != 1 ||
		bridge.reserve(uuid.NewString()) {
		t.Fatal("permanent persistence failure returned capacity")
	}
}

func TestUsagePluginInvalidTokenPoisonsBridge(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 2)
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(governance.UsageAttempt) {
			t.Fatal("invalid token reached persistence")
		}),
		bridge,
		nil,
	)

	plugin.HandleUsage(context.Background(), sdkusage.Record{APIKey: "tampered"})

	if !bridge.poisoned() || bridge.reserve(uuid.NewString()) {
		t.Fatal("invalid callback did not poison admission")
	}
}
