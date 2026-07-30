package command

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/cliproxy"
	"github.com/clemsix6/LLMGW/internal/adapter/discord"
	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// alertDrainTimeout is the budget every caller gives the webhook to deliver
// what is still queued before the process exits.
const alertDrainTimeout = 5 * time.Second

// Bounded classifications carried by the stopping event. The returned error is
// never rendered: it wraps store and lock failures carrying DSN material, which
// must not reach Discord.
const (
	stoppingContextCancelled = "context_cancelled"
	stoppingServiceReturned  = "service_returned"
	stoppingStartupFailure   = "startup_failure"
)

// newServeAlerting builds the tracker every serve observation point shares and
// the webhook its deferred shutdown drains.
//
// A malformed webhook URL fails serve. When alerting is disabled the notifier
// stays a nil interface rather than a typed nil pointer: a typed nil would make
// every hot-path observation take the tracker mutex for nothing.
func newServeAlerting(
	ctx context.Context,
	cfg config.Config,
	streams Streams,
) (*alert.Tracker, *discord.Webhook, error) {
	webhookURL, enabled, err := cfg.DiscordWebhookURL(streams.Getenv)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve serve alerting:\n%w", err)
	}
	if !enabled {
		log.Print("llmgw: Discord alerting is disabled")
		return alert.New(nil, nil, alert.DefaultWindow, time.Now), nil, nil
	}

	warnForeignWebhookHost(webhookURL)
	webhook := discord.New(webhookURL, nil, time.Now)
	tracker := alert.New(webhook, credentialLabels(ctx, cfg), alert.DefaultWindow, time.Now)
	return tracker, webhook, nil
}

// stopAlerting reports why serve is returning and drains what is queued under a
// bounded budget.
//
// The budget runs on a fresh context: the serve context is already cancelled by
// the time this runs, and a drain on a dead context delivers nothing — losing
// the one event the drain exists for. An incomplete drain is logged and nothing
// more, so alerting can never change the process exit status.
func stopAlerting(
	ctx context.Context,
	tracker *alert.Tracker,
	webhook *discord.Webhook,
	serviceStarted bool,
) {
	tracker.Emit(alert.KindGatewayStopping, alert.Field{
		Name:  "Reason",
		Value: stoppingReason(ctx, serviceStarted),
	})

	drainCtx, cancel := context.WithTimeout(context.Background(), alertDrainTimeout)
	defer cancel()
	if err := webhook.Close(drainCtx); err != nil {
		log.Printf("llmgw: discord alert drain incomplete: %v", err)
	}
}

// stoppingReason classifies one serve return from control flow alone, because
// the returned error is not reportable and has already been joined with the
// lock-release outcome by the time the stopping event is built.
func stoppingReason(ctx context.Context, serviceStarted bool) string {
	switch {
	case ctx.Err() != nil:
		return stoppingContextCancelled
	case serviceStarted:
		return stoppingServiceReturned
	default:
		return stoppingStartupFailure
	}
}

// credentialLabels renders provider credential IDs as the operator-facing names
// alert events carry.
//
// Disabled entries are included: the map only renders names, and a disabled
// credential's events should still read well. A lookup failure degrades to an
// unlabelled tracker and never fails startup.
func credentialLabels(ctx context.Context, cfg config.Config) map[string]alert.CredentialLabel {
	if cfg.Proxy == nil {
		return nil
	}
	auths, err := cliproxy.ListAuth(ctx, cfg.Proxy.AuthDir)
	if err != nil {
		log.Printf("llmgw: discord alert credential labels unavailable: %v", err)
		return nil
	}

	labels := make(map[string]alert.CredentialLabel, len(auths))
	for _, auth := range auths {
		labels[auth.ID] = alert.CredentialLabel{Provider: auth.Provider, Label: auth.Label}
	}
	return labels
}

// warnForeignWebhookHost logs the one accepted-with-a-warning configuration: a
// self-hosted relay is legitimate, so the host is not part of the contract.
func warnForeignWebhookHost(webhookURL string) {
	parsed, err := url.Parse(webhookURL)
	if err != nil {
		return
	}
	switch parsed.Hostname() {
	case "discord.com", "discordapp.com":
		return
	}
	log.Printf("llmgw: Discord alerting posts to non-Discord host %q", parsed.Hostname())
}

// operatorNotifier delivers one operator event from a short-lived command.
//
// It is deliberately two-phase: construction resolves the webhook URL so a
// command can fail before doing its work, while emit runs after the action,
// when the created key or the login result exists.
type operatorNotifier struct {
	url     string  // url is the resolved webhook endpoint, empty while alerting is disabled.
	enabled bool    // enabled reports whether an event is delivered at all.
	streams Streams // streams receives the one warning a failed drain produces.
}

// newOperatorNotifier resolves the webhook URL before its command acts.
//
// A malformed value fails the command while nothing has been created, rotated,
// or imported yet. A disabled configuration yields a working no-op notifier, so
// call sites never branch.
func newOperatorNotifier(cfg config.Config, streams Streams) (*operatorNotifier, error) {
	webhookURL, enabled, err := cfg.DiscordWebhookURL(streams.Getenv)
	if err != nil {
		return nil, fmt.Errorf("resolve operator alerting:\n%w", err)
	}
	return &operatorNotifier{
		url:     webhookURL,
		enabled: enabled,
		streams: normalizedStreams(streams),
	}, nil
}

// emit delivers one operator event and drains it under a bounded budget.
//
// It never fails the command: the key was created, the credential was added.
// Only an incomplete drain is observable here — Tracker.Emit does not report
// the queue's answer back, and a per-delivery failure is logged by the adapter
// itself — so that is the one condition warned about, on stderr.
func (n *operatorNotifier) emit(kind alert.Kind, fields ...alert.Field) {
	if n == nil || !n.enabled {
		return
	}

	webhook := discord.New(n.url, nil, time.Now)
	alert.New(webhook, nil, alert.DefaultWindow, time.Now).Emit(kind, fields...)

	ctx, cancel := context.WithTimeout(context.Background(), alertDrainTimeout)
	defer cancel()
	if err := webhook.Close(ctx); err != nil {
		fmt.Fprintf(n.streams.Err, "llmgw: discord alert not delivered: %v\n", err)
	}
}

// createdKeyFields renders the operator-facing identity of a created or rotated
// project key. The plaintext value of the created key is deliberately absent: a
// project key must never reach Discord.
func createdKeyFields(created governance.CreatedKey) []alert.Field {
	fields := []alert.Field{
		{Name: "Project", Value: created.Key.ProjectName},
		{Name: "Key", Value: created.Key.Name},
		{Name: "Public ID", Value: created.Key.PublicID},
	}
	if created.Key.ExpiresAt == nil {
		return fields
	}
	return append(fields, alert.Field{
		Name:  "Expires at",
		Value: created.Key.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// credentialAddedFields renders the credential one login produced. Its file
// path is deliberately absent: a filesystem location is not among what may
// leave the machine, even though the command prints it locally.
func credentialAddedFields(info cliproxy.AuthInfo) []alert.Field {
	return []alert.Field{
		{Name: "Provider", Value: info.Provider},
		{Name: "Label", Value: info.Label},
	}
}

// importedCredentialsFields summarises one legacy import as counts, because the
// import emits a single summary event rather than one event per credential.
func importedCredentialsFields(results []cliproxy.LegacyImport) []alert.Field {
	statuses := make(map[string]int, len(results))
	for _, result := range results {
		statuses[result.Status]++
	}
	return []alert.Field{
		{Name: "Credentials", Value: strconv.Itoa(len(results))},
		{Name: "Imported", Value: strconv.Itoa(statuses["imported"])},
		{Name: "Already present", Value: strconv.Itoa(statuses["exists"])},
		{Name: "Needs login", Value: strconv.Itoa(statuses["needs-login"])},
	}
}
