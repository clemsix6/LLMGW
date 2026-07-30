package command

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/cliproxy"
	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// operatorKeyPlaintext is the one-time key secret the operator field helpers
// must never render. It is deliberately recognisable inside a whole payload.
const operatorKeyPlaintext = "llmgw_plaintext_must_never_leak"

// operatorConfig builds the minimal configuration the operator notifier reads.
func operatorConfig(envName string) config.Config {
	return config.Config{LLMGW: config.LLMGW{DiscordWebhookURLEnv: envName}}
}

// TestNewOperatorNotifierRejectsMalformedURL proves the resolving phase fails
// its command while nothing has been created, rotated or imported yet.
func TestNewOperatorNotifierRejectsMalformedURL(t *testing.T) {
	streams := testRootStreams(map[string]string{testWebhookEnv: "webhook.discord.com/api/webhooks/1"})

	if _, err := newOperatorNotifier(operatorConfig(testWebhookEnv), streams); err == nil {
		t.Fatal("newOperatorNotifier accepted a malformed webhook URL")
	}
}

// TestNewOperatorNotifierDisabledStaysUsable proves a disabled configuration
// yields a working no-op notifier and no error, which is what keeps every call
// site free of a branch — and that the no-op really performs no request.
func TestNewOperatorNotifierDisabledStaysUsable(t *testing.T) {
	tests := []struct {
		name        string
		envName     string
		environment map[string]string
	}{
		{name: "no variable named", envName: "", environment: nil},
		{
			name:        "variable blank",
			envName:     testWebhookEnv,
			environment: map[string]string{testWebhookEnv: "   "},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := newWebhookStub(t)

			notifier, err := newOperatorNotifier(
				operatorConfig(test.envName),
				testRootStreams(test.environment),
			)
			if err != nil {
				t.Fatalf("newOperatorNotifier: %v", err)
			}
			if notifier == nil {
				t.Fatal("newOperatorNotifier returned no notifier")
			}

			notifier.emit(alert.KindProjectKeyCreated, alert.Field{Name: "Project", Value: "billing"})
			if delivered := stub.received(); len(delivered) != 0 {
				t.Fatalf("deliveries = %d, want 0", len(delivered))
			}
		})
	}
}

// TestOperatorNotifierEmitsOneEvent proves the emitting phase delivers what its
// command produced. It is also the control that makes the disabled case's
// "nothing arrived" assertion mean something.
func TestOperatorNotifierEmitsOneEvent(t *testing.T) {
	stub := newWebhookStub(t)
	streams := testRootStreams(map[string]string{testWebhookEnv: stub.server.URL})

	notifier, err := newOperatorNotifier(operatorConfig(testWebhookEnv), streams)
	if err != nil {
		t.Fatalf("newOperatorNotifier: %v", err)
	}
	notifier.emit(alert.KindProjectKeyCreated, alert.Field{Name: "Project", Value: "billing"})

	embeds := decodeDeliveries(t, stub.waitFor(t, 1))
	if len(embeds) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(embeds))
	}
	if embeds[0].Title != alert.KindProjectKeyCreated.Title() {
		t.Fatalf("title = %q, want %q", embeds[0].Title, alert.KindProjectKeyCreated.Title())
	}
	if got := embedFieldValue(embeds[0], "Project"); got != "billing" {
		t.Fatalf("Project = %q, want %q", got, "billing")
	}
}

// TestOperatorNotifierEmitSurvivesAnUnreachableWebhook proves a delivery that
// cannot happen never fails its command.
//
// It asserts silence rather than a warning: against a refused connection the
// delivery goroutine logs the drop through the standard logger and Close still
// reports a complete drain, so emit's stderr warning — reserved for a drain
// budget that expired mid-drain — legitimately never fires here.
func TestOperatorNotifierEmitSurvivesAnUnreachableWebhook(t *testing.T) {
	streams := testRootStreams(map[string]string{testWebhookEnv: unreachableWebhookURL(t)})
	errOut := new(bytes.Buffer)
	streams.Err = errOut

	notifier, err := newOperatorNotifier(operatorConfig(testWebhookEnv), streams)
	if err != nil {
		t.Fatalf("newOperatorNotifier: %v", err)
	}

	runWithin(t, 2*alertDrainTimeout, func() {
		notifier.emit(alert.KindCredentialAdded, alert.Field{Name: "Provider", Value: "claude"})
	})
	if errOut.Len() != 0 {
		t.Fatalf("emit wrote %q to stderr, want nothing", errOut.String())
	}
}

// TestCredentialLabelMapKeysEveryCredentialByItsID proves the one conversion
// that makes a credential alert readable: without it an event names an opaque
// file, and a provider/label swap or a wrong key would mislabel every one of
// them with nothing else in the tree noticing.
func TestCredentialLabelMapKeysEveryCredentialByItsID(t *testing.T) {
	auths := []cliproxy.AuthInfo{
		{ID: "claude-ops-example-com.json", Provider: "claude", Label: "ops@example.com"},
		// Disabled entries are kept: the map only renders names, and a disabled
		// credential's events should still read well.
		{ID: "codex-ci-example-com.json", Provider: "codex", Label: "ci@example.com", Disabled: true},
	}

	labels := credentialLabelMap(auths)
	if len(labels) != len(auths) {
		t.Fatalf("labels = %d, want %d", len(labels), len(auths))
	}
	for _, auth := range auths {
		label, ok := labels[auth.ID]
		if !ok {
			t.Fatalf("credential %q is unlabelled", auth.ID)
		}
		if label.Provider != auth.Provider {
			t.Fatalf("provider = %q, want %q", label.Provider, auth.Provider)
		}
		if label.Label != auth.Label {
			t.Fatalf("label = %q, want %q", label.Label, auth.Label)
		}
	}
}

// TestCredentialLabelMapWithoutCredentialsIsEmpty proves the empty auth
// directory every fresh deployment starts with yields a usable map rather than
// a surprise.
func TestCredentialLabelMapWithoutCredentialsIsEmpty(t *testing.T) {
	if labels := credentialLabelMap(nil); len(labels) != 0 {
		t.Fatalf("labels = %d, want 0", len(labels))
	}
}

// TestCreatedKeyFieldsRenderIdentityWithoutThePlaintext proves the operator
// event names the key, carries the expiry only when there is one, and never
// carries the one-time secret the command printed locally.
func TestCreatedKeyFieldsRenderIdentityWithoutThePlaintext(t *testing.T) {
	expiresAt := time.Date(2027, time.March, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		expiresAt  *time.Time
		wantExpiry string
	}{
		{name: "without expiry", expiresAt: nil, wantExpiry: ""},
		{name: "with expiry", expiresAt: &expiresAt, wantExpiry: expiresAt.Format(time.RFC3339)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields := createdKeyFields(createdKeyFixture(test.expiresAt))

			assertFieldValues(t, fields, map[string]string{
				"Project":   "billing",
				"Key":       "ci",
				"Public ID": "pk_operator",
			})
			if got := alertFieldValue(fields, "Expires at"); got != test.wantExpiry {
				t.Fatalf("Expires at = %q, want %q", got, test.wantExpiry)
			}
			assertNoFieldContains(t, fields, operatorKeyPlaintext)
		})
	}
}

// createdKeyFixture builds one created key carrying a recognisable plaintext.
func createdKeyFixture(expiresAt *time.Time) governance.CreatedKey {
	return governance.CreatedKey{
		Key: governance.KeyInfo{
			ProjectName: "billing",
			Name:        "ci",
			PublicID:    "pk_operator",
			ExpiresAt:   expiresAt,
		},
		Plaintext: operatorKeyPlaintext,
	}
}

// TestCredentialAddedFieldsCarryOnlyTheIdentity proves the login event names the
// provider and the account and nothing else: the local file the command prints
// is not among what may leave the machine.
func TestCredentialAddedFieldsCarryOnlyTheIdentity(t *testing.T) {
	info := cliproxy.AuthInfo{
		ID:       "claude-ops-example-com.json",
		Provider: "claude",
		Label:    "ops@example.com",
		Disabled: true,
	}

	fields := credentialAddedFields(info)
	if len(fields) != 2 {
		t.Fatalf("fields = %d, want 2", len(fields))
	}
	assertFieldValues(t, fields, map[string]string{
		"Provider": "claude",
		"Label":    "ops@example.com",
	})
	assertNoFieldContains(t, fields, info.ID)
}

// TestImportedCredentialsFieldsSummariseOneImport proves the legacy import
// produces a single counted summary across the three statuses ImportLegacy
// really returns, rather than one event per credential.
func TestImportedCredentialsFieldsSummariseOneImport(t *testing.T) {
	results := []cliproxy.LegacyImport{
		{Provider: "claude", Label: "first", Status: "imported"},
		{Provider: "claude", Label: "second", Status: "imported"},
		{Provider: "codex", Label: "third", Status: "exists"},
		{Provider: "codex", Label: "fourth", Status: "needs-login"},
	}

	fields := importedCredentialsFields(results)
	if len(fields) != 4 {
		t.Fatalf("fields = %d, want 4", len(fields))
	}
	assertFieldValues(t, fields, map[string]string{
		"Credentials":     "4",
		"Imported":        "2",
		"Already present": "1",
		"Needs login":     "1",
	})
}

// assertFieldValues fails unless every wanted field is rendered with its value.
func assertFieldValues(t *testing.T, fields []alert.Field, want map[string]string) {
	t.Helper()

	for name, value := range want {
		if got := alertFieldValue(fields, name); got != value {
			t.Fatalf("field %q = %q, want %q", name, got, value)
		}
	}
}

// assertNoFieldContains fails when any rendered value carries the secret.
func assertNoFieldContains(t *testing.T, fields []alert.Field, secret string) {
	t.Helper()

	for _, field := range fields {
		if strings.Contains(field.Value, secret) {
			t.Fatalf("field %q carries %q", field.Name, secret)
		}
	}
}
