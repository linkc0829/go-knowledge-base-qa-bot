package gateway

import "errors"

var (
	// ErrUnauthorized indicates missing or invalid bearer token credentials.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrUserLimit indicates the effective user has exceeded their concurrent inflight quota.
	ErrUserLimit = errors.New("user concurrency limit exceeded")
	// ErrGlobalLimit indicates the entire gateway has reached maximum concurrent capacity.
	ErrGlobalLimit = errors.New("global concurrency limit exceeded")
	// ErrPayloadTooLarge indicates the request body exceeded maximum permissible size.
	ErrPayloadTooLarge = errors.New("payload too large")
	// ErrInvalidMaxTokens indicates an output limit given as a non-numeric string.
	ErrInvalidMaxTokens = errors.New("invalid max_tokens")
	// ErrMultipleSequences indicates a request asking the upstream for more than one sequence.
	ErrMultipleSequences = errors.New("multiple sequences not supported")
)
