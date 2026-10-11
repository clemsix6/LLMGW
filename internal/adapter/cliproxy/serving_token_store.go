package cliproxy

import (
	"context"
	"fmt"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// servingTokenStore is the SDK file store used while serving. The SDK applies a credential's
// file-level routing hints only from its file watcher, which LLMGW disables, so the store
// applies them itself when the manager loads credentials. Everything else is the plain
// SDK file store, including its optional interfaces.
type servingTokenStore struct {
	*sdkauth.FileTokenStore
}

// newServingTokenStore returns an unconfigured serving store; the SDK builder sets its base directory.
func newServingTokenStore() *servingTokenStore {
	return &servingTokenStore{FileTokenStore: sdkauth.NewFileTokenStore()}
}

// List loads the credentials and makes each file's priority effective for routing.
func (s *servingTokenStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	auths, err := s.FileTokenStore.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list serving credentials:\n%w", err)
	}
	for _, auth := range auths {
		if auth != nil {
			coreauth.ApplyAuthPriorityMetadata(auth, auth.Metadata)
		}
	}
	return auths, nil
}

// registerServingTokenStore makes the SDK builder load credentials through the serving store.
func registerServingTokenStore() {
	sdkauth.RegisterTokenStore(newServingTokenStore())
}
