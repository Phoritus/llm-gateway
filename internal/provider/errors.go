package provider

import (
	"errors"
	"regexp"
	"strconv"
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

	// ErrModelUnavailable is a credential that works but may not use the
	// model: the upstream does not know it, or this account may not call it.
	ErrModelUnavailable = errors.New("upstream model unavailable to this credential")

	// ErrInvalidRequest is the upstream refusing the request itself (a 400 or
	// 422 that is not about context length).
	ErrInvalidRequest = errors.New("upstream rejected the request as invalid")
)

// Error is an upstream failure with what the caller needs beyond its kind.
// Detail is for logs only: it can quote the upstream's own message, which may
// name the account, so it is never sent to a client.
type Error struct {
	Kind       error
	RetryAfter time.Duration // set with ErrRateLimited when the upstream said
	Detail     string
	// Window and Requested are the context window and the request's size, in
	// the upstream's tokens, that an ErrContextTooLong stated. Zero when it
	// did not say.
	Window, Requested int
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

// ContextLimit returns the window and request size a context-too-long error
// stated, or zeros.
func ContextLimit(err error) (window, requested int) {
	var e *Error
	if errors.As(err, &e) {
		return e.Window, e.Requested
	}
	return 0, 0
}

// ContextTooLong is the error for an upstream message saying the request does
// not fit, with the numbers it states read out of it.
func ContextTooLong(message, detail string) *Error {
	window, requested := parseContextLimit(message)
	return &Error{Kind: ErrContextTooLong, Detail: detail, Window: window, Requested: requested}
}

// contextLimits are the ways the upstreams state a window and a request size.
// Each pattern names its numbers window and requested.
var contextLimits = []*regexp.Regexp{
	// OpenAI, OpenRouter, Groq, vLLM: "maximum context length is 8192 tokens.
	// However, your messages resulted in 9000 tokens" / "you requested about
	// 140000 tokens".
	regexp.MustCompile(`(?i)maximum context length is (?P<window>\d+)(?:.*?(?:resulted in|requested(?: about)?) (?P<requested>\d+))?`),
	// Anthropic: "prompt is too long: 210000 tokens > 200000 maximum".
	regexp.MustCompile(`(?i)(?P<requested>\d+) tokens > (?P<window>\d+) maximum`),
	// Mistral: "Prompt contains 40000 tokens and 0 draft tokens, too large for
	// model with 32768 maximum context length".
	regexp.MustCompile(`(?i)contains (?P<requested>\d+) tokens.*?model with (?P<window>\d+) maximum context length`),
	// Gemini: "The input token count (1200000) exceeds the maximum number of
	// tokens allowed (1048576)".
	regexp.MustCompile(`(?i)input token count \((?P<requested>\d+)\) exceeds the maximum number of tokens allowed \((?P<window>\d+)\)`),
	// Cerebras: "Current length is 9000 while limit is 8192".
	regexp.MustCompile(`(?i)current length is (?P<requested>\d+) while limit is (?P<window>\d+)`),
}

func parseContextLimit(message string) (window, requested int) {
	for _, re := range contextLimits {
		m := re.FindStringSubmatch(message)
		if m == nil {
			continue
		}
		window, _ = strconv.Atoi(m[re.SubexpIndex("window")])
		requested, _ = strconv.Atoi(m[re.SubexpIndex("requested")])
		return window, requested
	}
	return 0, 0
}
