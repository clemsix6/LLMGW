package cliproxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

func TestCredential(t *testing.T) {
	tests := []struct {
		name    string
		headers func() http.Header
		want    string
		wantErr bool
	}{
		{
			name: "bearer",
			headers: func() http.Header {
				return http.Header{"Authorization": {"Bearer secret-a"}}
			},
			want: "secret-a",
		},
		{
			name: "x api key",
			headers: func() http.Header {
				return http.Header{"X-Api-Key": {"secret-a"}}
			},
			want: "secret-a",
		},
		{
			name: "matching dual headers",
			headers: func() http.Header {
				return http.Header{
					"Authorization": {"Bearer secret-a"},
					"X-Api-Key":     {"secret-a"},
				}
			},
			want: "secret-a",
		},
		{
			name: "missing",
			headers: func() http.Header {
				return http.Header{}
			},
			wantErr: true,
		},
		{
			name: "wrong scheme",
			headers: func() http.Header {
				return http.Header{"Authorization": {"Basic secret-a"}}
			},
			wantErr: true,
		},
		{
			name: "empty bearer",
			headers: func() http.Header {
				return http.Header{"Authorization": {"Bearer "}}
			},
			wantErr: true,
		},
		{
			name: "duplicate authorization",
			headers: func() http.Header {
				return http.Header{"Authorization": {"Bearer secret-a", "Bearer secret-a"}}
			},
			wantErr: true,
		},
		{
			name: "duplicate x api key",
			headers: func() http.Header {
				return http.Header{"X-Api-Key": {"secret-a", "secret-a"}}
			},
			wantErr: true,
		},
		{
			name: "divergent dual headers",
			headers: func() http.Header {
				return http.Header{
					"Authorization": {"Bearer secret-a"},
					"X-Api-Key":     {"secret-b"},
				}
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := credential(test.headers())
			if test.wantErr {
				if err == nil {
					t.Fatalf("credential = %q, nil; want error", got)
				}
				if strings.Contains(err.Error(), "secret-a") || strings.Contains(err.Error(), "secret-b") {
					t.Fatalf("credential error leaked a key: %v", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("credential = %q, %v; want %q, nil", got, err, test.want)
			}
		})
	}
}

func TestAccessAuthenticateRequiresRequestIdentity(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}

	result, authErr := (AccessProvider{}).Authenticate(context.Background(), request)
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
	if authErr == nil || authErr.Code != sdkaccess.AuthErrorCodeNoCredentials {
		t.Fatalf("auth error = %#v, want no credentials", authErr)
	}
}

func TestAccessAuthenticateReturnsSafeSDKPrincipal(t *testing.T) {
	bridge := fixedUsageBridge(t)
	identity := RequestIdentity{
		RequestID:   "f5efc3a8-e6c3-49fd-bad6-6532fa51d216",
		ProjectID:   42,
		ClientKeyID: 7,
		KeyPublicID: "MDEyMzQ1Njc4OWFi",
		Operation:   governance.OperationGeneration,
	}
	request, err := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	request = request.WithContext(WithIdentity(request.Context(), identity))

	result, authErr := (AccessProvider{bridge: bridge}).Authenticate(context.Background(), request)
	if authErr != nil {
		t.Fatalf("Authenticate error = %v", authErr)
	}
	if result.Provider != AccessProviderType ||
		result.Principal == identity.KeyPublicID ||
		result.Principal == "" {
		t.Fatalf("result = %#v", result)
	}
	if correlation, ok := bridge.correlation(result.Principal); !ok ||
		correlation.requestID != identity.RequestID ||
		correlation.keyPublicID != identity.KeyPublicID {
		t.Fatalf("principal correlation = (%#v, %t)", correlation, ok)
	}
	if len(result.Metadata) != 0 {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestRegisterExclusiveAccessRestoresOtherProviders(t *testing.T) {
	other := staticAccessProvider{id: "other"}
	sdkaccess.RegisterProvider(other.id, other)
	defer sdkaccess.UnregisterProvider(other.id)
	defer sdkaccess.ClearExclusiveProvider()
	defer sdkaccess.UnregisterProvider(AccessProviderType)

	provider := staticAccessProvider{id: "custom-llmgw"}
	clear := RegisterExclusiveAccess(provider)

	registered := sdkaccess.RegisteredProviders()
	if len(registered) != 1 || registered[0] != provider {
		t.Fatalf("registered providers = %#v, want only supplied LLMGW provider", registered)
	}

	clear()
	registered = sdkaccess.RegisteredProviders()
	if len(registered) != 1 || registered[0] != other {
		t.Fatalf("providers after cleanup = %#v, want only unrelated provider", registered)
	}
}

type staticAccessProvider struct {
	id string
}

func (p staticAccessProvider) Identifier() string {
	return p.id
}

func (staticAccessProvider) Authenticate(
	context.Context,
	*http.Request,
) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return nil, sdkaccess.NewNoCredentialsError()
}
