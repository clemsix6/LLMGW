package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// fakeStore is a minimal domain.Store for handler tests.
type fakeStore struct {
	projectID int64 // projectID is returned by EnsureProject.

	recorded usage.Usage // recorded captures the last RecordUsage call.
}

// EnsureProject returns the configured projectID.
func (s *fakeStore) EnsureProject(_ context.Context, _ string) (int64, error) {
	return s.projectID, nil
}

// LimitsFor returns no limits (no budget enforcement).
func (s *fakeStore) LimitsFor(_ context.Context, _ int64, _ string) ([]domain.BudgetLimit, error) {
	return nil, nil
}

// PriceFor reports no price row.
func (s *fakeStore) PriceFor(_ context.Context, _ string) (float64, float64, bool, error) {
	return 0, 0, false, nil
}

// DefaultRoute returns nil (handler tests inject the provider directly).
func (s *fakeStore) DefaultRoute(_ context.Context) (domain.Provider, error) {
	return nil, nil
}

// RecordUsage captures the token counts from the event.
func (s *fakeStore) RecordUsage(_ context.Context, e domain.UsageEvent) error {
	s.recorded = usage.Usage{InputTokens: e.InputTokens, OutputTokens: e.OutputTokens}
	return nil
}

// ReserveIfAdmitted always admits without a reservation.
func (s *fakeStore) ReserveIfAdmitted(_ context.Context, _ int64, _ string, _ time.Duration, _ []domain.WindowRead, admit func([]domain.WindowTotals) bool) (int64, bool, error) {
	return 0, admit(nil), nil
}

// ReleaseReservation is a no-op.
func (s *fakeStore) ReleaseReservation(_ context.Context, _ int64) error {
	return nil
}

// fakeProvider is a minimal domain.Provider for handler tests.
type fakeProvider struct {
	body  []byte      // body is written to the sink on Send.
	usage usage.Usage // usage is returned by Send.
}

// Send writes body to out and returns the configured usage.
func (p *fakeProvider) Send(_ context.Context, _ llm.Request, out domain.StreamSink) (usage.Usage, error) {
	_, _ = out.Write(p.body)
	out.Flush()
	return p.usage, nil
}

// TestHandlerForwardsAndRecords verifies that the handler relays the provider response and
// records usage from the provider's metered tokens.
func TestHandlerForwardsAndRecords(t *testing.T) {
	store := &fakeStore{projectID: 7}
	prov := &fakeProvider{body: []byte(`{"ok":true}`), usage: usage.Usage{InputTokens: 3, OutputTokens: 5}}
	h := newHandler(store, prov, AnthropicWire{}, "claude-max", "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("X-Project", "p")
	h.handle(rec, req)

	if rec.Code != http.StatusOK || store.recorded.OutputTokens != 5 {
		t.Fatalf("status=%d recorded=%+v", rec.Code, store.recorded)
	}
}
