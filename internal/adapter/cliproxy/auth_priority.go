package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// priorityMetadataKey is the credential-file key the SDK selector reads as the routing tier.
const priorityMetadataKey = "priority"

// metadataPriority returns the routing tier the SDK derives from a credential file's metadata,
// by running the SDK's own parser on a throwaway auth so the listed auth is never touched.
// A missing or invalid priority counts as zero.
func metadataPriority(metadata map[string]any) int {
	raw, present := metadata[priorityMetadataKey]
	if !present {
		return 0
	}
	probe := &coreauth.Auth{}
	coreauth.ApplyAuthPriorityMetadata(probe, map[string]any{priorityMetadataKey: raw})
	priority, err := strconv.Atoi(probe.Attributes[priorityMetadataKey])
	if err != nil {
		return 0
	}
	return priority
}

// authInfoOf projects one loaded auth to its non-secret operator view.
func authInfoOf(auth *coreauth.Auth) AuthInfo {
	return AuthInfo{
		ID:       auth.ID,
		Provider: auth.Provider,
		Label:    auth.Label,
		Disabled: auth.Disabled,
		Priority: metadataPriority(auth.Metadata),
	}
}

// SetAuthPriority sets the routing priority of one local auth file, or removes it when priority is zero.
// Every other key of the file is kept.
func SetAuthPriority(ctx context.Context, authDir, id string, priority int) (AuthInfo, error) {
	if err := PrepareAuthDir(authDir); err != nil {
		return AuthInfo{}, err
	}
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		return AuthInfo{}, err
	}
	auth, err := findAuth(ctx, store, id)
	if err != nil {
		return AuthInfo{}, err
	}
	updated := withPriority(auth, priority)
	// Creation intent is required because the stage directory starts empty: without it the SDK
	// store skips saving a disabled credential whose file it does not find there. This rewrites an
	// existing operator-owned file, so nothing new is created.
	if _, err := store.Save(coreauth.WithAuthCreationIntent(ctx), updated); err != nil {
		return AuthInfo{}, fmt.Errorf("set auth priority: save failed:\n%w", err)
	}
	// The SDK parser ignores a Go int, so the written value is reported directly.
	info := authInfoOf(updated)
	info.Priority = priority
	return info, nil
}

// findAuth returns the listed auth with the exact ID.
func findAuth(ctx context.Context, store *secureFileTokenStore, id string) (*coreauth.Auth, error) {
	auths, err := store.List(ctx)
	if err != nil {
		return nil, errors.New("set auth priority: list local auth unavailable")
	}
	for _, auth := range auths {
		if auth != nil && auth.ID == id {
			return auth, nil
		}
	}
	return nil, errors.New("set auth priority: unknown auth id")
}

// withPriority returns a copy of auth that the secure store accepts for saving: the location
// attributes set by List are dropped because the store derives the destination from FileName.
func withPriority(auth *coreauth.Auth, priority int) *coreauth.Auth {
	updated := cloneAuthForStage(auth)
	delete(updated.Attributes, coreauth.AttributePath)
	delete(updated.Attributes, coreauth.AttributeSource)
	delete(updated.Attributes, coreauth.AttributeSourceBackend)
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	if priority == 0 {
		delete(updated.Metadata, priorityMetadataKey)
		return updated
	}
	updated.Metadata[priorityMetadataKey] = priority
	return updated
}
