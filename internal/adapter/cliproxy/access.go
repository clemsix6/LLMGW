package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

// AccessProviderType is the exclusive SDK access-provider registration key.
const AccessProviderType = "llmgw-project-key"

// errCredential is deliberately generic and never contains supplied header values.
var errCredential = errors.New("invalid credential")

// AccessProvider bridges an identity authenticated by LLMGW into SDK access control.
type AccessProvider struct {
	bridge *UsageBridge
}

// Identifier returns the stable LLMGW SDK access-provider name.
func (AccessProvider) Identifier() string {
	return AccessProviderType
}

// Authenticate accepts only a request identity already attached by LLMGW.
func (p AccessProvider) Authenticate(
	_ context.Context,
	request *http.Request,
) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if request == nil {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	identity, ok := IdentityFromContext(request.Context())
	if !ok {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	principal, ok := p.bridge.principal(identity)
	if !ok {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	return &sdkaccess.Result{
		Provider:  AccessProviderType,
		Principal: principal,
	}, nil
}

// RegisterExclusiveAccess installs the only SDK provider allowed by LLMGW.
func RegisterExclusiveAccess(provider sdkaccess.Provider) func() {
	sdkaccess.RegisterProvider(AccessProviderType, provider)
	sdkaccess.SetExclusiveProvider(AccessProviderType)

	var once sync.Once
	return func() {
		once.Do(func() {
			sdkaccess.ClearExclusiveProvider()
			sdkaccess.UnregisterProvider(AccessProviderType)
		})
	}
}

// credential extracts one unambiguous project key from supported headers.
func credential(headers http.Header) (string, error) {
	authorization := headers.Values("Authorization")
	apiKeys := headers.Values("X-Api-Key")
	if len(authorization) > 1 || len(apiKeys) > 1 {
		return "", errCredential
	}

	bearer, err := bearerCredential(authorization)
	if err != nil {
		return "", err
	}
	apiKey, err := apiKeyCredential(apiKeys)
	if err != nil {
		return "", err
	}
	if bearer == "" && apiKey == "" {
		return "", errCredential
	}
	if bearer != "" && apiKey != "" && bearer != apiKey {
		return "", errCredential
	}
	if bearer != "" {
		return bearer, nil
	}
	return apiKey, nil
}

// bearerCredential parses at most one Authorization Bearer value.
func bearerCredential(values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	scheme, value, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || !validCredentialValue(value) {
		return "", errCredential
	}
	return value, nil
}

// apiKeyCredential validates at most one x-api-key value.
func apiKeyCredential(values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	if !validCredentialValue(values[0]) {
		return "", errCredential
	}
	return values[0], nil
}

// validCredentialValue rejects empty or whitespace-bearing credential values.
func validCredentialValue(value string) bool {
	return value != "" && !strings.ContainsAny(value, " \t\r\n")
}
