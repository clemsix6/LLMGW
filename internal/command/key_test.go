package command

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
)

// TestKeyCommandLifecycleKeepsPlaintextOutOfMetadata catches a command mutation that leaks a
// generated credential or its digest through list, revoke, or error output.
func TestKeyCommandLifecycleKeepsPlaintextOutOfMetadata(t *testing.T) {
	dsn := commandStore(t)
	streams := commandStreams(t, dsn)
	ctx := context.Background()
	beforeExpiry := time.Now().UTC().Add(71*time.Hour + 59*time.Minute)
	if err := runKey(ctx, []string{"create", "truewallet", "--name", "server-1", "--expires", "72h"}, streams); err != nil {
		t.Fatalf("key create: %v", err)
	}
	afterExpiry := time.Now().UTC().Add(72*time.Hour + time.Minute)
	created := streams.Out.(*bytes.Buffer).String()
	plaintext := keyFromOutput(t, created)
	if !strings.Contains(created, plaintext) {
		t.Fatal("create output omitted plaintext")
	}

	store := openCommandStore(t, dsn)
	persisted, err := store.ListKeys(ctx, "truewallet")
	if err != nil || len(persisted) != 1 {
		t.Fatalf("persisted keys = (%d, %v), want 1", len(persisted), err)
	}
	if persisted[0].ExpiresAt == nil || persisted[0].ExpiresAt.Before(beforeExpiry) || persisted[0].ExpiresAt.After(afterExpiry) {
		t.Fatalf("persisted expiry = %v, want 72h lifetime", persisted[0].ExpiresAt)
	}
	if bytes.Equal(persisted[0].Digest, []byte(plaintext)) {
		t.Fatal("stored digest equals plaintext bytes")
	}
	digestHex := hex.EncodeToString(persisted[0].Digest)
	digestBase64 := base64.StdEncoding.EncodeToString(persisted[0].Digest)

	streams.Out.(*bytes.Buffer).Reset()
	if err := runKey(ctx, []string{"list", "truewallet"}, streams); err != nil {
		t.Fatalf("key list: %v", err)
	}
	listed := streams.Out.(*bytes.Buffer).String()
	if !strings.Contains(listed, "server-1") || strings.Contains(listed, plaintext) || strings.Contains(listed, digestHex) || strings.Contains(listed, digestBase64) || strings.Contains(strings.ToLower(listed), "digest") {
		t.Fatalf("list output leaked or omitted metadata: %q", listed)
	}
	id := keyIDFromOutput(t, listed)
	streams.Out.(*bytes.Buffer).Reset()
	beforeOverlap := time.Now().UTC().Add(23*time.Hour + 59*time.Minute)
	if err := runKey(ctx, []string{"rotate", id, "--overlap", "24h"}, streams); err != nil {
		t.Fatalf("key rotate: %v", err)
	}
	afterOverlap := time.Now().UTC().Add(24*time.Hour + time.Minute)
	rotated := streams.Out.(*bytes.Buffer).String()
	replacementPlaintext := keyFromOutput(t, rotated)
	if replacementPlaintext == plaintext {
		t.Fatal("rotation repeated old plaintext")
	}
	replacementID := keyIDFromOutput(t, rotated)
	persisted, err = store.ListKeys(ctx, "truewallet")
	if err != nil || len(persisted) != 2 {
		t.Fatalf("rotated keys = (%d, %v), want 2", len(persisted), err)
	}
	if persisted[0].ExpiresAt == nil || persisted[0].ExpiresAt.Before(beforeOverlap) || persisted[0].ExpiresAt.After(afterOverlap) || persisted[0].RevokedAt != nil {
		t.Fatalf("old key retirement = expires %v revoked %v, want 24h overlap", persisted[0].ExpiresAt, persisted[0].RevokedAt)
	}
	if persisted[1].ID == persisted[0].ID || persisted[1].ExpiresAt != nil || persisted[1].RevokedAt != nil {
		t.Fatalf("replacement state = %#v, want distinct active key", persisted[1])
	}
	streams.Out.(*bytes.Buffer).Reset()
	if err := runKey(ctx, []string{"revoke", replacementID}, streams); err != nil {
		t.Fatalf("key revoke: %v", err)
	}
	if strings.Contains(streams.Out.(*bytes.Buffer).String(), plaintext) || strings.Contains(streams.Err.(*bytes.Buffer).String(), plaintext) {
		t.Fatal("revoke output leaked plaintext")
	}
	replacementNumericID, _ := strconv.ParseInt(replacementID, 10, 64)
	revoked, err := store.KeyByID(ctx, replacementNumericID)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("persisted revocation = (%v, %v), want timestamp", revoked.RevokedAt, err)
	}

	streams.Out.(*bytes.Buffer).Reset()
	if err := runKey(ctx, []string{"create", "second-project", "--name", "global-key"}, streams); err != nil {
		t.Fatalf("create global-list key: %v", err)
	}
	secondPlaintext := keyFromOutput(t, streams.Out.(*bytes.Buffer).String())
	streams.Out.(*bytes.Buffer).Reset()
	if err := runKey(ctx, []string{"list"}, streams); err != nil {
		t.Fatalf("global key list: %v", err)
	}
	global := streams.Out.(*bytes.Buffer).String()
	if !strings.Contains(global, "truewallet") || !strings.Contains(global, "second-project") || !strings.Contains(global, "server-1") || !strings.Contains(global, "global-key") {
		t.Fatalf("global key list omitted projects or names: %q", global)
	}
	if strings.Contains(global, plaintext) || strings.Contains(global, replacementPlaintext) || strings.Contains(global, secondPlaintext) || strings.Contains(global, digestHex) {
		t.Fatalf("global key list leaked secret material: %q", global)
	}
}

// TestKeyCommandReportsPlaintextDeliveryFailures catches a mutation that discards the output
// error after persistence and falsely reports successful credential delivery.
func TestKeyCommandReportsPlaintextDeliveryFailures(t *testing.T) {
	dsn := commandStore(t)
	streams := commandStreams(t, dsn)
	ctx := context.Background()

	t.Run("create remains persisted", func(t *testing.T) {
		writer := &secretRejectingWriter{}
		var errorOutput, logs bytes.Buffer
		streams.Out, streams.Err = writer, &errorOutput
		restoreLogs := captureCommandLogs(&logs)
		defer restoreLogs()
		err := runKey(ctx, []string{"create", "create-output-failure", "--name", "created"}, streams)
		if err == nil || !strings.Contains(err.Error(), "write created key") || !errors.Is(err, errSecretDelivery) {
			t.Fatalf("create error = %v, want contextual secret delivery error", err)
		}
		assertNoSecretText(t, errorOutput.String(), logs.String())
		store := openCommandStore(t, dsn)
		keys, err := store.ListKeys(ctx, "create-output-failure")
		if err != nil || len(keys) != 1 {
			t.Fatalf("persisted keys = (%d, %v), want 1", len(keys), err)
		}
	})

	t.Run("rotation transition remains durable", func(t *testing.T) {
		streams.Out, streams.Err = new(bytes.Buffer), new(bytes.Buffer)
		if err := runKey(ctx, []string{"create", "rotate-output-failure", "--name", "rotated"}, streams); err != nil {
			t.Fatalf("create old key: %v", err)
		}
		store := openCommandStore(t, dsn)
		oldKeys, err := store.ListKeys(ctx, "rotate-output-failure")
		if err != nil || len(oldKeys) != 1 {
			t.Fatalf("old keys = (%d, %v)", len(oldKeys), err)
		}
		writer := &secretRejectingWriter{}
		var errorOutput, logs bytes.Buffer
		streams.Out, streams.Err = writer, &errorOutput
		restoreLogs := captureCommandLogs(&logs)
		defer restoreLogs()
		before := time.Now().UTC().Add(23*time.Hour + 59*time.Minute)
		err = runKey(ctx, []string{"rotate", strconv.FormatInt(oldKeys[0].ID, 10), "--overlap", "24h"}, streams)
		after := time.Now().UTC().Add(24*time.Hour + time.Minute)
		if err == nil || !strings.Contains(err.Error(), "write rotated key") || !errors.Is(err, errSecretDelivery) {
			t.Fatalf("rotate error = %v, want contextual secret delivery error", err)
		}
		assertNoSecretText(t, errorOutput.String(), logs.String())
		keys, err := store.ListKeys(ctx, "rotate-output-failure")
		if err != nil || len(keys) != 2 {
			t.Fatalf("keys after output failure = (%d, %v), want durable replacement", len(keys), err)
		}
		if keys[0].ExpiresAt == nil || keys[0].ExpiresAt.Before(before) || keys[0].ExpiresAt.After(after) {
			t.Fatalf("old key expiry after output failure = %v, want durable 24h overlap", keys[0].ExpiresAt)
		}
		if keys[1].ID == keys[0].ID || keys[1].RevokedAt != nil {
			t.Fatalf("replacement after output failure = %#v", keys[1])
		}
	})
}

var errSecretDelivery = errors.New("reject plaintext delivery")

// secretRejectingWriter accepts metadata but refuses the only plaintext credential line.
type secretRejectingWriter struct{}

// Write implements io.Writer and rejects writes containing a plaintext project key.
func (w *secretRejectingWriter) Write(value []byte) (int, error) {
	if bytes.Contains(value, []byte("key\tllmgw_k_")) {
		return 0, errSecretDelivery
	}
	return len(value), nil
}

// captureCommandLogs redirects the process logger for one serial security assertion.
func captureCommandLogs(output *bytes.Buffer) func() {
	previous := log.Writer()
	log.SetOutput(output)
	return func() { log.SetOutput(previous) }
}

// assertNoSecretText proves error and log channels contain no project credential material.
func assertNoSecretText(t *testing.T, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(value, "llmgw_k_") {
			t.Fatalf("secret leaked outside successful output: %q", value)
		}
	}
}

// openCommandStore opens a second repository connection for persisted CLI assertions.
func openCommandStore(t *testing.T, dsn string) *postgres.Store {
	t.Helper()
	store, err := postgres.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open command assertion store: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// keyFromOutput extracts the one credential emitted by a successful create or rotate command.
func keyFromOutput(t *testing.T, output string) string {
	t.Helper()
	match := regexp.MustCompile(`llmgw_k_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`).FindString(output)
	if match == "" {
		t.Fatalf("output contained no project key: %q", output)
	}
	return match
}

// keyIDFromOutput extracts a numeric persisted key ID from non-secret key-list output.
func keyIDFromOutput(t *testing.T, output string) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^id\s+([0-9]+)`).FindStringSubmatch(output)
	if len(match) != 2 {
		t.Fatalf("output contained no key ID: %q", output)
	}
	return match[1]
}
