package command

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
	"github.com/clemsix6/LLMGW/internal/domain/projectkey"
)

// runKey executes one local project-key command without starting the gateway or management API.
func runKey(ctx context.Context, args []string, streams Streams) error {
	streams = normalizedStreams(streams)
	if len(args) == 0 {
		return keyUsage(streams, "missing key command")
	}
	switch args[0] {
	case "create":
		return runKeyCreate(ctx, args[1:], streams)
	case "list":
		return runKeyList(ctx, args[1:], streams)
	case "rotate":
		return runKeyRotate(ctx, args[1:], streams)
	case "revoke":
		return runKeyRevoke(ctx, args[1:], streams)
	default:
		return keyUsage(streams, fmt.Sprintf("unknown key command %q", args[0]))
	}
}

// runKeyCreate creates the named project if absent and emits its one-time plaintext key.
func runKeyCreate(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("key create", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	name := flags.String("name", "", "operator-facing key name")
	expires := flags.Duration("expires", 0, "key lifetime")
	project, err := parseRequiredTarget(flags, args)
	if err != nil {
		return err
	}
	if project == "" || flags.NArg() != 0 || *name == "" || *expires < 0 {
		return keyUsage(streams, "key create requires --name and a non-negative --expires")
	}
	cfg, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	// Resolved before the key exists, so a malformed webhook variable can never
	// leave a created key behind.
	notifier, err := newOperatorNotifier(cfg, streams)
	if err != nil {
		return err
	}
	service, err := keyService(cfg, store, streams)
	if err != nil {
		return err
	}
	var expiry *time.Time
	if *expires > 0 {
		value := time.Now().UTC().Add(*expires)
		expiry = &value
	}
	created, err := service.Create(ctx, project, *name, expiry)
	if err != nil {
		return fmt.Errorf("create key:\n%w", err)
	}
	if err := printCreatedKey(streams.Out, created); err != nil {
		return fmt.Errorf("write created key:\n%w", err)
	}
	notifier.emit(alert.KindProjectKeyCreated, createdKeyFields(created)...)
	return nil
}

// runKeyList emits digest-free key metadata for one existing project or every project.
func runKeyList(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("key list", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	project, err := parseOptionalTarget(flags, args)
	if err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return keyUsage(streams, "key list accepts at most PROJECT")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	if project != "" {
		if err := requireProject(ctx, store, project); err != nil {
			return err
		}
	}
	keys, err := store.ListKeys(ctx, project)
	if err != nil {
		return fmt.Errorf("list keys:\n%w", err)
	}
	for _, key := range keys {
		if err := printKeyInfo(streams.Out, keyInfo(key)); err != nil {
			return fmt.Errorf("write key list:\n%w", err)
		}
	}
	return nil
}

// runKeyRotate replaces one existing key and emits only the replacement plaintext value.
func runKeyRotate(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("key rotate", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	overlap := flags.Duration("overlap", 0, "old key overlap")
	keyText, err := parseRequiredTarget(flags, args)
	if err != nil {
		return err
	}
	if keyText == "" || flags.NArg() != 0 || *overlap < 0 {
		return keyUsage(streams, "key rotate requires a non-negative --overlap")
	}
	keyID, err := strconv.ParseInt(keyText, 10, 64)
	if err != nil || keyID < 1 {
		return keyUsage(streams, "key rotate requires a positive KEY_ID")
	}
	cfg, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	// Resolved before the rotation, so a malformed webhook variable can never
	// leave a half-rotated key behind.
	notifier, err := newOperatorNotifier(cfg, streams)
	if err != nil {
		return err
	}
	if _, err := store.KeyByID(ctx, keyID); err != nil {
		return fmt.Errorf("find key %d:\n%w", keyID, err)
	}
	service, err := keyService(cfg, store, streams)
	if err != nil {
		return err
	}
	created, err := service.Rotate(ctx, keyID, *overlap)
	if err != nil {
		return fmt.Errorf("rotate key:\n%w", err)
	}
	if err := printCreatedKey(streams.Out, created); err != nil {
		return fmt.Errorf("write rotated key:\n%w", err)
	}
	notifier.emit(alert.KindProjectKeyRotated, createdKeyFields(created)...)
	return nil
}

// runKeyRevoke immediately revokes one existing key without loading the key pepper.
func runKeyRevoke(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("key revoke", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	keyText, err := parseRequiredTarget(flags, args)
	if err != nil {
		return err
	}
	if keyText == "" || flags.NArg() != 0 {
		return keyUsage(streams, "key revoke requires KEY_ID")
	}
	keyID, err := strconv.ParseInt(keyText, 10, 64)
	if err != nil || keyID < 1 {
		return keyUsage(streams, "key revoke requires a positive KEY_ID")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := store.KeyByID(ctx, keyID); err != nil {
		return fmt.Errorf("find key %d:\n%w", keyID, err)
	}
	revokedAt := time.Now().UTC()
	if err := store.RevokeKey(ctx, keyID, revokedAt); err != nil {
		return fmt.Errorf("revoke key:\n%w", err)
	}
	if _, err := fmt.Fprintf(streams.Out, "id\t%d\nrevoked\t%s\n", keyID, revokedAt.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("write revoked key:\n%w", err)
	}
	return nil
}

// keyService opens the pepper-dependent lifecycle service only for key creation or rotation.
func keyService(cfg config.Config, store *postgres.Store, streams Streams) (*projectkey.Service, error) {
	pepper, err := cfg.KeyPepper(streams.Getenv)
	if err != nil {
		return nil, fmt.Errorf("load key pepper:\n%w", err)
	}
	service, err := projectkey.NewService(store, pepper, rand.Reader, func() time.Time { return time.Now().UTC() })
	if err != nil {
		return nil, fmt.Errorf("create key service:\n%w", err)
	}
	return service, nil
}

// printCreatedKey emits the one permitted plaintext key representation.
func printCreatedKey(output io.Writer, created governance.CreatedKey) error {
	if err := printKeyInfo(output, created.Key); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "key\t%s\n", created.Plaintext); err != nil {
		return fmt.Errorf("write plaintext key:\n%w", err)
	}
	return nil
}

// printKeyInfo emits only non-secret project key fields.
func printKeyInfo(output io.Writer, key governance.KeyInfo) error {
	if _, err := fmt.Fprintf(output, "id\t%d\nproject\t%s\nname\t%s\npublic_id\t%s\n", key.ID, key.ProjectName, key.Name, key.PublicID); err != nil {
		return fmt.Errorf("write key metadata:\n%w", err)
	}
	if err := printOptionalTime(output, "expires_at", key.ExpiresAt); err != nil {
		return err
	}
	return printOptionalTime(output, "revoked_at", key.RevokedAt)
}

// keyInfo projects a persisted key to its non-secret fields for command output.
func keyInfo(key governance.ClientKey) governance.KeyInfo {
	return governance.KeyInfo{ID: key.ID, ProjectID: key.ProjectID, ProjectName: key.ProjectName, Name: key.Name, PublicID: key.PublicID, CreatedAt: key.CreatedAt, ExpiresAt: key.ExpiresAt, RevokedAt: key.RevokedAt, LastUsedAt: key.LastUsedAt}
}

// printOptionalTime emits a stable empty value for absent timestamps.
func printOptionalTime(output io.Writer, label string, value *time.Time) error {
	if value == nil {
		_, err := fmt.Fprintf(output, "%s\t\n", label)
		return err
	}
	_, err := fmt.Fprintf(output, "%s\t%s\n", label, value.UTC().Format(time.RFC3339))
	return err
}

// keyUsage writes a short usage line for a leaf-parser error.
func keyUsage(streams Streams, message string) error {
	fmt.Fprintln(streams.Err, "usage: key {create|list|rotate|revoke}")
	return fmt.Errorf("%s", message)
}
