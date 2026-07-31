package cliproxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	usageBridgeKeyBytes      = 32
	usagePublicIDBytes       = 12
	usagePublicIDTextBytes   = 16
	usagePrincipalPrefix     = "llmgw_usage_v1"
	usageBarrierPrefix       = "llmgw_usage_barrier_v1"
	usageCancelPrefix        = "llmgw_usage_cancel_v1"
	usagePayloadBytes        = 16 + usagePublicIDTextBytes
	usageBarrierPayloadBytes = 17
	usageCancelPayloadBytes  = 16
	// usageParkTimeout bounds a canceled barrier's wait for its producer. The
	// producer may never publish — a request canceled before it reached an
	// executor, or while the SDK waited out a cooldown, publishes nothing — so
	// an unbounded wait leaks the permit silently until capacity is gone.
	usageParkTimeout = 10 * time.Second
	// usageExpiredFloor is the minimum number of expired parked groups kept so
	// a straggler record persists instead of reading as a compatibility failure.
	usageExpiredFloor = 128
)

// UsageBridge authenticates immutable request correlation carried by the SDK
// usage record. One process-local bridge must be shared by AccessProvider and
// UsagePlugin.
type UsageBridge struct {
	key         [usageBridgeKeyBytes]byte
	capacity    int
	parkTimeout time.Duration // parkTimeout bounds how long a canceled barrier may wait for its producer.

	mu            sync.Mutex
	active        map[string]*usageGroupState
	expired       map[string]struct{} // expired remembers parked groups released by timeout.
	expiredOrder  []string            // expiredOrder evicts the oldest tombstone at the limit.
	expiredLimit  int                 // expiredLimit bounds tombstone retention.
	isPoisoned    bool
	changed       chan struct{}
	publishRecord func(context.Context, sdkusage.Record)
	onPoison      func() // onPoison reports the terminal state exactly once.
}

// usageGroupState distinguishes durable bootstrap attempts from the one
// producer that may publish after a canceled downstream stream returns.
type usageGroupState struct {
	failed               bool
	cancelSeen           bool
	barrierSeen          bool
	canceled             bool
	successBeforeBarrier bool
	primaryAfterCancel   bool
}

// usageCorrelation is the verified subset required for durable attribution.
type usageCorrelation struct {
	requestID   string
	keyPublicID string
}

// NewUsageBridge reads an independent process-local HMAC key.
func NewUsageBridge(random io.Reader, capacity int) (*UsageBridge, error) {
	if random == nil {
		return nil, fmt.Errorf("usage bridge random source is required")
	}
	if capacity <= 0 {
		return nil, fmt.Errorf("usage bridge capacity must be positive")
	}
	bridge := &UsageBridge{
		capacity:      capacity,
		parkTimeout:   usageParkTimeout,
		active:        make(map[string]*usageGroupState, capacity),
		expired:       make(map[string]struct{}),
		expiredLimit:  max(4*capacity, usageExpiredFloor),
		changed:       make(chan struct{}),
		publishRecord: sdkusage.PublishRecord,
	}
	if _, err := io.ReadFull(random, bridge.key[:]); err != nil {
		return nil, fmt.Errorf("read usage bridge key:\n%w", err)
	}
	return bridge, nil
}

// reserve non-blockingly owns one generation group before admission.
func (b *UsageBridge) reserve(requestID string) bool {
	if b == nil {
		return false
	}
	id, err := uuid.Parse(requestID)
	if err != nil || id.String() != requestID {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.isPoisoned || len(b.active) >= b.capacity {
		return false
	}
	if _, exists := b.active[requestID]; exists {
		return false
	}
	b.active[requestID] = &usageGroupState{}
	b.signalLocked()
	return true
}

// fail keeps the named permit held after an admitted attempt could not be
// persisted. Other already-admitted groups may still drain.
//
// A failed group is never released: its usage is unaccounted, so returning the
// permit would let the gateway keep spending against a budget it can no longer
// measure. That makes the loss cumulative over the life of the process, and
// once failures fill every permit the bridge can neither admit nor drain
// anything. Report that as terminal rather than refusing every generation from
// behind a healthy status route: the observer stops the service so its manager
// restarts it, and reconciliation resolves the unaccounted requests.
func (b *UsageBridge) fail(requestID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	group, exists := b.active[requestID]
	if !exists {
		b.mu.Unlock()
		return
	}
	group.failed = true
	b.signalLocked()
	stalled := b.stalledLocked()
	b.mu.Unlock()
	if stalled {
		b.poison()
	}
}

// stalledLocked reports that no permit can be reserved or returned again.
func (b *UsageBridge) stalledLocked() bool {
	if len(b.active) < b.capacity {
		return false
	}
	for _, group := range b.active {
		if !group.failed {
			return false
		}
	}
	return true
}

// release returns one healthy permit exactly once. A poisoned bridge or a
// failed group remains fail-closed.
func (b *UsageBridge) release(requestID string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	group, exists := b.active[requestID]
	if b.isPoisoned || !exists || group.failed {
		return false
	}
	delete(b.active, requestID)
	b.signalLocked()
	return true
}

// acceptRecord rejects an authenticated record after its bounded group was
// released. This is a process-wide compatibility failure, not a new attempt —
// except a straggler from an expired parked group, which is the producer the
// parking window waited for: it persists normally, just without a permit.
func (b *UsageBridge) acceptRecord(requestID string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.isPoisoned {
		return false
	}
	if _, exists := b.active[requestID]; exists {
		return true
	}
	if _, expired := b.expired[requestID]; expired {
		return true
	}
	b.isPoisoned = true
	b.signalLocked()
	return false
}

// persisted records durable callback order. A canceled group that crossed its
// barrier completes on the next durable record, because configuration forbids
// auxiliary-model records: image generation is disabled on every endpoint and
// payload write rules that could reinject the tool are rejected at startup.
func (b *UsageBridge) persisted(requestID string, failed bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	group, exists := b.active[requestID]
	if !exists || b.isPoisoned || group.failed {
		return
	}
	if group.barrierSeen && group.canceled {
		delete(b.active, requestID)
		b.signalLocked()
		return
	}
	if group.cancelSeen {
		group.primaryAfterCancel = true
	}
	if !failed {
		group.successBeforeBarrier = true
	}
}

// completeBarrier applies normal FIFO completion or canceled-stream waiting.
// Parking is bounded: the producer a canceled stream waits for may never
// publish, so a timer returns the permit and leaves a tombstone instead.
func (b *UsageBridge) completeBarrier(requestID string, canceled bool) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	group, exists := b.active[requestID]
	if b.isPoisoned || !exists || group.failed || group.barrierSeen {
		return false
	}
	if !canceled || group.successBeforeBarrier || group.primaryAfterCancel {
		delete(b.active, requestID)
		b.signalLocked()
		return true
	}
	group.barrierSeen = true
	group.canceled = true
	b.signalLocked()
	time.AfterFunc(b.parkTimeout, func() { b.expirePark(requestID) })
	return false
}

// expirePark returns a parked canceled group's permit once its producer has
// had the whole parking window to publish. The tombstone keeps a straggler
// record persistable without reading it as a compatibility failure. A group
// that completed, failed, or was never parked is left exactly as it is.
func (b *UsageBridge) expirePark(requestID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	group, exists := b.active[requestID]
	if !exists || !group.barrierSeen || !group.canceled || group.failed {
		b.mu.Unlock()
		return
	}
	delete(b.active, requestID)
	b.rememberExpiredLocked(requestID)
	b.signalLocked()
	b.mu.Unlock()
	log.Printf("llmgw: canceled generation produced no usage record (request=%s): permit returned", requestID)
}

// rememberExpiredLocked stores one tombstone, evicting the oldest at the limit.
func (b *UsageBridge) rememberExpiredLocked(requestID string) {
	if len(b.expiredOrder) >= b.expiredLimit {
		delete(b.expired, b.expiredOrder[0])
		b.expiredOrder = b.expiredOrder[1:]
	}
	b.expired[requestID] = struct{}{}
	b.expiredOrder = append(b.expiredOrder, requestID)
}

func (b *UsageBridge) markCanceled(requestID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if group, exists := b.active[requestID]; exists && !group.failed {
		group.cancelSeen = true
		b.signalLocked()
	}
	b.mu.Unlock()
}

// poison rejects future generations and prevents any later barrier from
// returning capacity after an unauthenticated SDK callback. The state is
// terminal: it is reported once so the process can stop instead of serving
// nothing but 503 until an operator notices.
func (b *UsageBridge) poison() {
	if b == nil {
		return
	}
	b.mu.Lock()
	report := b.onPoison
	newlyPoisoned := !b.isPoisoned
	if newlyPoisoned {
		b.isPoisoned = true
		b.signalLocked()
	}
	b.mu.Unlock()
	if newlyPoisoned && report != nil {
		report()
	}
}

// ReportPoisonWith installs the one terminal-state observer. It must be called
// during composition, before the service starts serving.
func (b *UsageBridge) ReportPoisonWith(report func()) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.onPoison = report
	b.mu.Unlock()
}

func (b *UsageBridge) poisoned() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.isPoisoned
}

func (b *UsageBridge) outstanding() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.active)
}

func (b *UsageBridge) waitDrained(ctx context.Context) error {
	if b == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		b.mu.Lock()
		if len(b.active) == 0 {
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *UsageBridge) signalLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// publishBarrier appends the authenticated end-of-generation marker to the
// same global SDK FIFO as executor usage records.
func (b *UsageBridge) publishBarrier(requestID string, canceledValues ...bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	_, reserved := b.active[requestID]
	publish := b.publishRecord
	b.mu.Unlock()
	if !reserved || publish == nil {
		b.poison()
		return
	}
	canceled := len(canceledValues) > 0 && canceledValues[0]
	token, ok := b.barrierFor(requestID, canceled)
	if !ok {
		b.poison()
		return
	}
	publish(context.Background(), sdkusage.Record{APIKey: token})
}

// publishCancel appends a marker before cancellation becomes visible to the
// SDK, separating completed failovers from the one active stream producer.
func (b *UsageBridge) publishCancel(requestID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	_, reserved := b.active[requestID]
	publish := b.publishRecord
	b.mu.Unlock()
	if !reserved || publish == nil {
		b.poison()
		return
	}
	token, ok := b.cancel(requestID)
	if !ok {
		b.poison()
		return
	}
	publish(context.Background(), sdkusage.Record{APIKey: token})
}

func (b *UsageBridge) cancel(requestID string) (string, bool) {
	if b == nil {
		return "", false
	}
	id, err := uuid.Parse(requestID)
	if err != nil || id.String() != requestID {
		return "", false
	}
	encoded := base64.RawURLEncoding.EncodeToString(id[:])
	unsigned := usageCancelPrefix + "." + encoded
	return unsigned + "." + b.signature(unsigned), true
}

func (b *UsageBridge) cancelRequestID(token string) (string, bool) {
	if b == nil {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != usageCancelPrefix {
		return "", false
	}
	unsigned := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(signature) != sha256.Size ||
		!hmac.Equal(signature, b.signatureBytes(unsigned)) {
		return "", false
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(payload) != usageCancelPayloadBytes {
		return "", false
	}
	requestID, err := uuid.FromBytes(payload)
	if err != nil {
		return "", false
	}
	return requestID.String(), true
}

func (b *UsageBridge) barrierFor(requestID string, canceled bool) (string, bool) {
	if b == nil {
		return "", false
	}
	id, err := uuid.Parse(requestID)
	if err != nil || id.String() != requestID {
		return "", false
	}
	payload := make([]byte, usageBarrierPayloadBytes)
	copy(payload, id[:])
	if canceled {
		payload[usageBarrierPayloadBytes-1] = 1
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	unsigned := usageBarrierPrefix + "." + encoded
	return unsigned + "." + b.signature(unsigned), true
}

func (b *UsageBridge) barrierRequestID(token string) (string, bool) {
	requestID, _, ok := b.barrierState(token)
	return requestID, ok
}

func (b *UsageBridge) barrierState(token string) (string, bool, bool) {
	if b == nil {
		return "", false, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != usageBarrierPrefix {
		return "", false, false
	}
	unsigned := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(signature) != sha256.Size ||
		!hmac.Equal(signature, b.signatureBytes(unsigned)) {
		return "", false, false
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(payload) != usageBarrierPayloadBytes ||
		payload[usageBarrierPayloadBytes-1] > 1 {
		return "", false, false
	}
	requestID, err := uuid.FromBytes(payload[:16])
	if err != nil {
		return "", false, false
	}
	return requestID.String(), payload[usageBarrierPayloadBytes-1] == 1, true
}

// principal creates the only accepted SDK principal from an admitted identity.
func (b *UsageBridge) principal(identity RequestIdentity) (string, bool) {
	if b == nil || !validUsagePublicID(identity.KeyPublicID) {
		return "", false
	}
	requestID, err := uuid.Parse(identity.RequestID)
	if err != nil || requestID.String() != identity.RequestID {
		return "", false
	}

	var payload [usagePayloadBytes]byte
	copy(payload[:16], requestID[:])
	copy(payload[16:], identity.KeyPublicID)
	encoded := base64.RawURLEncoding.EncodeToString(payload[:])
	unsigned := usagePrincipalPrefix + "." + encoded
	return unsigned + "." + b.signature(unsigned), true
}

// correlation verifies and parses one immutable SDK principal fail-closed.
func (b *UsageBridge) correlation(principal string) (usageCorrelation, bool) {
	if b == nil {
		return usageCorrelation{}, false
	}
	parts := strings.Split(principal, ".")
	if len(parts) != 3 || parts[0] != usagePrincipalPrefix {
		return usageCorrelation{}, false
	}
	unsigned := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(signature) != sha256.Size ||
		!hmac.Equal(signature, b.signatureBytes(unsigned)) {
		return usageCorrelation{}, false
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(payload) != usagePayloadBytes {
		return usageCorrelation{}, false
	}
	requestID, err := uuid.FromBytes(payload[:16])
	if err != nil {
		return usageCorrelation{}, false
	}
	publicID := string(payload[16:])
	if !validUsagePublicID(publicID) {
		return usageCorrelation{}, false
	}
	return usageCorrelation{
		requestID:   requestID.String(),
		keyPublicID: publicID,
	}, true
}

func (b *UsageBridge) signature(value string) string {
	return base64.RawURLEncoding.EncodeToString(b.signatureBytes(value))
}

func (b *UsageBridge) signatureBytes(value string) []byte {
	mac := hmac.New(sha256.New, b.key[:])
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

// validUsagePublicID enforces the project-key public identifier wire shape.
func validUsagePublicID(publicID string) bool {
	if len(publicID) != usagePublicIDTextBytes {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(publicID)
	return err == nil && len(decoded) == usagePublicIDBytes
}
