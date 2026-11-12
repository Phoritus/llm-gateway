package provider

import (
	"errors"
	"time"
)

// The kinds of upstream failure. Adapters classify what the upstream said
// into one of these so the router picks a skip rule with errors.Is instead of
// parsing messages. Every adapter error that is not a cancellation wraps one.
var (
	ErrAuth           = errors.New("upstream rejected the credential")
	ErrQuota          = errors.New("upstream quota exhausted")
	ErrRateLimited    = errors.New("upstream rate limited the request")
	ErrContextTooLong = errors.New("request exceeds the model's context window")
	ErrUpstream       = errors.New("upstream failed")
	ErrTimeout        = errors.New("upstream timed out")
)

// Error is an upstream failure with what the caller needs beyond its kind.
// Detail is for logs only: it can quote the upstream's own message, which may
// name the account, so it is never sent to a client.
type Error struct {
	Kind       error
	RetryAfter time.Duration // set with ErrRateLimited when the upstream said
	Detail     string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Kind.Error()
	}
	return e.Kind.Error() + ": " + e.Detail
}

func (e *Error) Unwrap() error { return e.Kind }

// RetryAfter reports how long the upstream asked to be left alone, or zero.
func RetryAfter(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}
