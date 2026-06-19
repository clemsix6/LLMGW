package domain

import "time"

// ProviderError is a provider failure the gateway maps to HTTP without knowing the concrete
// provider. RetryAfter's bool is false when no Retry-After header applies.
type ProviderError interface {
	error

	// HTTPStatus is the status to send the client.
	HTTPStatus() int

	// ErrorType is the stable machine-readable type.
	ErrorType() string

	// RetryAfter is the backoff when one is known.
	RetryAfter() (time.Duration, bool)
}
