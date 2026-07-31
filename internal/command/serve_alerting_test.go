package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// The serve fixture's DSN carries a recognisable user and host. The returned
// error wraps store and lock failures, and spec §12 forbids that material from
// reaching Discord: a substring search over the whole payload is what proves it.
const (
	leakedDSNUser = "dsn-leak-user"
	leakedDSNHost = "dsn-leak-host.invalid"
)

// syncBuffer collects standard-logger output written from any goroutine.
type syncBuffer struct {
	mu     sync.Mutex   // mu guards buffer against the delivery and worker goroutines.
	buffer bytes.Buffer // buffer accumulates the captured lines.
}

// Write appends one log line.
func (b *syncBuffer) Write(line []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(line)
}

// String returns what was captured so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

// captureLog redirects the standard logger for the duration of one test, which
// is where the composition reports what it accepted with a warning.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()

	captured := &syncBuffer{}
	previous := log.Writer()
	log.SetOutput(captured)
	t.Cleanup(func() { log.SetOutput(previous) })
	return captured
}

// alertingServeConfig builds the serve fixture with alerting enabled.
//
// The auth directory is a temporary root rather than the shared fixture's
// absolute path: enabling the webhook makes runServeWith read the credential
// labels through ListAuth, which really creates the directory and bypasses the
// stubbed prepareAuthDir seam entirely.
func alertingServeConfig(t *testing.T) config.Config {
	t.Helper()

	cfg := validServeConfig()
	cfg.Proxy = &sdkconfig.Config{AuthDir: t.TempDir()}
	cfg.LLMGW.DiscordWebhookURLEnv = testWebhookEnv
	return cfg
}

// alertingEnvironment extends the serve environment with the webhook variable
// and a DSN whose user and host are recognisable in any delivered payload.
func alertingEnvironment(webhookURL string) map[string]string {
	environment := serveEnvironment()
	environment["TEST_DSN"] = fmt.Sprintf(
		"postgres://%s:dsn-leak-secret@%s:5432/llmgw",
		leakedDSNUser,
		leakedDSNHost,
	)
	environment[testWebhookEnv] = webhookURL
	return environment
}

// TestServeWithAlertingUnsetTouchesNoWebhook proves the disabled configuration
// costs nothing on the wire: the composition builds and runs without a single
// outbound delivery.
func TestServeWithAlertingUnsetTouchesNoWebhook(t *testing.T) {
	stub := newWebhookStub(t)
	cfg := alertingServeConfig(t)
	environment := alertingEnvironment(stub.server.URL)
	environment[testWebhookEnv] = ""

	err := runServeWith(
		context.Background(),
		nil,
		testRootStreams(environment),
		successfulServeDependencies(&cfg, nil),
	)
	if err == nil {
		t.Fatal("serve succeeded")
	}
	if delivered := stub.received(); len(delivered) != 0 {
		t.Fatalf("deliveries = %d, want 0", len(delivered))
	}
}

// TestServeAlertingLifecycle pins the two events serve owns and the bounded
// classification the stopping one carries.
//
// All three classifications are driven through the injected seams. The alerting
// defer sits after prepareAuthDir and before the store block, so every return
// above it emits nothing at all: only a seam registered after it can produce a
// startup failure.
func TestServeAlertingLifecycle(t *testing.T) {
	tests := []struct {
		name       string
		arrange    func(*serveDependencies, context.CancelFunc)
		wantReason string
		wantStart  bool
	}{
		{
			name:       "context cancelled",
			arrange:    arrangeCancelledRun,
			wantReason: stoppingContextCancelled,
			wantStart:  true,
		},
		{
			name:       "service returned",
			arrange:    arrangeFailingRun,
			wantReason: stoppingServiceReturned,
			wantStart:  true,
		},
		{
			name:       "startup failure",
			arrange:    arrangeFailingStore,
			wantReason: stoppingStartupFailure,
			wantStart:  false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := newWebhookStub(t)
			wantDeliveries := 1
			if test.wantStart {
				wantDeliveries = 2
			}

			embeds := runAlertingServe(t, stub, wantDeliveries, test.arrange)

			assertLifecycleEvents(t, embeds, test.wantReason, test.wantStart)
			assertNoDSNMaterial(t, stub.received())
		})
	}
}

// TestServeRejectsMalformedWebhookURL proves a typo in the alerting variable
// fails serve before the embedded service is ever constructed.
func TestServeRejectsMalformedWebhookURL(t *testing.T) {
	cfg := alertingServeConfig(t)
	builds := 0
	deps := successfulServeDependencies(&cfg, nil)
	deps.buildService = func(config.Config, *serveStore, []byte, *alert.Tracker) (serveService, error) {
		builds++
		return &fakeServeService{}, nil
	}

	streams := testRootStreams(alertingEnvironment("webhook.discord.com/api/webhooks/1"))
	if err := runServeWith(context.Background(), nil, streams, deps); err == nil {
		t.Fatal("serve accepted a malformed webhook URL")
	}
	if builds != 0 {
		t.Fatalf("SDK construction calls = %d, want 0", builds)
	}
}

// TestServeAcceptsNonDiscordWebhookHost proves a self-hosted relay is accepted
// with a warning rather than rejected: the host is not part of the contract.
func TestServeAcceptsNonDiscordWebhookHost(t *testing.T) {
	stub := newWebhookStub(t)
	captured := captureLog(t)

	embeds := runAlertingServe(t, stub, 2, arrangeFailingRun)

	assertLifecycleEvents(t, embeds, stoppingServiceReturned, true)
	host := stubWebhookHost(t, stub)
	if !strings.Contains(captured.String(), host) {
		t.Fatalf("startup logs do not warn about host %q: %s", host, captured.String())
	}
}

// runAlertingServe runs one serve with alerting pointed at the stub and returns
// the events it delivered, in delivery order.
func runAlertingServe(
	t *testing.T,
	stub *webhookStub,
	wantDeliveries int,
	arrange func(*serveDependencies, context.CancelFunc),
) []deliveredEmbed {
	t.Helper()

	cfg := alertingServeConfig(t)
	deps := successfulServeDependencies(&cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	arrange(&deps, cancel)

	streams := testRootStreams(alertingEnvironment(stub.server.URL))
	if err := runServeWith(ctx, nil, streams, deps); err == nil {
		t.Fatal("serve succeeded")
	}

	bodies := stub.waitFor(t, wantDeliveries)
	if len(bodies) != wantDeliveries {
		t.Fatalf("deliveries = %d, want %d", len(bodies), wantDeliveries)
	}
	return decodeDeliveries(t, bodies)
}

// arrangeCancelledRun makes the embedded service return because the serve
// context was cancelled under it.
func arrangeCancelledRun(deps *serveDependencies, cancel context.CancelFunc) {
	deps.buildService = func(config.Config, *serveStore, []byte, *alert.Tracker) (serveService, error) {
		return &fakeServeService{run: func(ctx context.Context) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
}

// arrangeFailingRun makes the embedded service return on its own while the
// serve context is still live.
func arrangeFailingRun(deps *serveDependencies, _ context.CancelFunc) {
	deps.buildService = func(config.Config, *serveStore, []byte, *alert.Tracker) (serveService, error) {
		return &fakeServeService{run: func(context.Context) error {
			return errors.New("embedded service failed")
		}}, nil
	}
}

// arrangeFailingStore fails a seam registered after the alerting defer, and
// wraps the DSN into the returned error the stopping event must not render.
func arrangeFailingStore(deps *serveDependencies, _ context.CancelFunc) {
	deps.openStore = func(_ context.Context, dsn string) (*serveStore, error) {
		return nil, fmt.Errorf("postgres unavailable for %s", dsn)
	}
}

// assertLifecycleEvents fails unless the delivered events are the expected
// gateway pair, in order, with the stopping one carrying the bounded reason.
func assertLifecycleEvents(t *testing.T, embeds []deliveredEmbed, reason string, started bool) {
	t.Helper()

	stopping := embeds[0]
	if started {
		if embeds[0].Title != alert.KindGatewayStarted.Title() {
			t.Fatalf("first title = %q, want %q", embeds[0].Title, alert.KindGatewayStarted.Title())
		}
		stopping = embeds[1]
	}
	if stopping.Title != alert.KindGatewayStopping.Title() {
		t.Fatalf("stopping title = %q, want %q", stopping.Title, alert.KindGatewayStopping.Title())
	}
	if got := embedFieldValue(stopping, "Reason"); got != reason {
		t.Fatalf("Reason = %q, want %q", got, reason)
	}
}

// assertNoDSNMaterial fails when any delivered payload carries connection
// material, whatever field it ended up in.
func assertNoDSNMaterial(t *testing.T, bodies []string) {
	t.Helper()

	for _, body := range bodies {
		for _, secret := range []string{leakedDSNUser, leakedDSNHost} {
			if strings.Contains(body, secret) {
				t.Fatalf("delivered payload carries %q: %s", secret, body)
			}
		}
	}
}

// stubWebhookHost returns the stub webhook's host, which is never a Discord one.
func stubWebhookHost(t *testing.T, stub *webhookStub) string {
	t.Helper()

	parsed, err := url.Parse(stub.server.URL)
	if err != nil {
		t.Fatalf("parse stub webhook URL: %v", err)
	}
	return parsed.Hostname()
}
