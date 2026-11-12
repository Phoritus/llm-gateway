# Step 2: mock provider streaming through /v1 (design sketch)

Status: implemented.

Deviation from the sketch: `Assemble(Stream)` became an `Assembler` fed one chunk
at a time (the Builder pattern), because the handler has already read the first
chunk before it knows it will assemble, and the exact cache in step 7 must
assemble while the same chunks stream to the client. The contract is otherwise
unchanged.

Deviation after `interrogate` (all four adversaries found the first two
independently): the deadline was only a context, so it did not stop a handler
blocked on the socket. A client that drips its body, or stops reading a
stream, held the request forever. The handler now also puts the deadline on
the socket through `http.ResponseController`: a read deadline over the body
(answered 408 `request_timeout` with `Connection: close`), and a write deadline
of the request deadline plus a 2s grace so the timeout event can still be sent.
Validation also now refuses a blank model, an unknown role, and content that is
not a string, an array of parts, or null. Value ranges (`max_tokens`,
`temperature`) stay the upstream's to judge, since they differ by provider.

## Usage first

```sh
curl -N localhost:8080/v1/chat/completions \
  -d '{"model":"mock/echo","stream":true,"messages":[{"role":"user","content":"hi there"}]}'
# data: {"id":"...","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}
# data: {"id":"...","object":"chat.completion.chunk","choices":[{"delta":{"content":" there"}}]}
# data: {... "finish_reason":"stop", "usage":{...}}
# data: [DONE]
```

The model name selects one of a fixed set of mock behaviours:

| model | behaviour |
| --- | --- |
| `mock/echo` | echoes the last user message word by word |
| `mock/slow` | the same with a delay before each chunk |
| `mock/rate-limited` | fails before streaming with `ErrRateLimited` and RetryAfter=7s |
| `mock/fail` | fails before streaming with `ErrUpstream` |
| `mock/context-too-long` | fails before streaming with `ErrContextTooLong` |
| `mock/break` | sends 3 chunks, then fails mid-stream |

The route that turns `model` into a target arrives in step 3. Until then `/v1` sends every
request to the mock.

## Types

```go
// internal/provider
type Provider interface {
    // ChatStream is the only call. A non-streaming request is the same stream
    // assembled by the gateway, and step 7's cache needs that same assembly anyway.
    ChatStream(ctx context.Context, req *ChatRequest, cred Credential) (Stream, error)
}

type Stream interface {
    Next() (*ChatChunk, error) // io.EOF at a clean end
    Close() error
}

type Credential struct{ APIKey string } // the mock ignores it; step 3 fills it

type ChatRequest struct {
    Model       string            `json:"model"`
    Messages    []Message         `json:"messages"`
    Stream      bool              `json:"stream,omitempty"`
    Temperature *float64          `json:"temperature,omitempty"`
    TopP        *float64          `json:"top_p,omitempty"`
    MaxTokens   *int              `json:"max_tokens,omitempty"`
    Stop        json.RawMessage   `json:"stop,omitempty"`
    Tools       json.RawMessage   `json:"tools,omitempty"`
    ToolChoice  json.RawMessage   `json:"tool_choice,omitempty"`
}
type Message struct {
    Role       string          `json:"role"`
    Content    json.RawMessage `json:"content"` // a string or an array of parts
    Name       string          `json:"name,omitempty"`
    ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
    ToolCallID string          `json:"tool_call_id,omitempty"`
}
type ChatChunk struct { ID, Object, Model string; Created int64; Choices []ChunkChoice; Usage *Usage }
type ChunkChoice struct { Index int; Delta Delta; FinishReason *string }
type Delta struct { Role, Content string; ToolCalls json.RawMessage }
type Usage struct { PromptTokens, CompletionTokens, TotalTokens int }
type ChatResponse struct { /* chat.completion shape, assembled from chunks */ }

// Typed errors: sentinel kinds plus one wrapper that carries what the router needs.
var ErrAuth, ErrQuota, ErrRateLimited, ErrContextTooLong, ErrUpstream, ErrTimeout error
type Error struct {
    Kind       error         // one of the sentinels; errors.Is works through Unwrap
    RetryAfter time.Duration // set for ErrRateLimited when known
    Message    string        // safe to log, never sent to the client verbatim
}

type Assembler struct{ ... } // Add(*ChatChunk); Response() *ChatResponse
```

```go
// internal/provider/mock
type Provider struct{ Delay time.Duration } // Delay applies to mock/slow
func (p *Provider) ChatStream(ctx, req, cred) (provider.Stream, error)
// Each Next waits on ctx, so a client disconnect ends the generation.
```

```go
// internal/gateway
type Deps struct { Provider provider.Provider; Timeout time.Duration; Logger *slog.Logger }
func Routes(d Deps) http.Handler // chi; POST /chat/completions; sets the telemetry route

handleChat:
  decode (4 MiB cap, unknown fields tolerated, model and messages required)
  ctx = WithTimeout(r.Context(), d.Timeout)
  stream, err := Provider.ChatStream(...)  -> err: writeProviderError
  first, err := stream.Next()               -> err: writeProviderError (no byte sent yet)
  -- commit point: everything below runs after the first byte --
  stream=false: Assemble(first + rest) -> 200 JSON  (an error here still sends an HTTP status)
  stream=true:  SSE headers, write first, flush; loop Next/write/flush;
                on error: write an error event and stop; on EOF: write data: [DONE]

writeProviderError: RateLimited -> 429 + Retry-After; ContextTooLong -> 413; anything else -> 502.
```

## Decisions

- **One provider method, not three.** `Embed` is Phase 2 and `Chat` duplicates the assembly
  step 7's cache needs. Each adapter then implements a single call.
- **Pull iterator, not a channel.** It needs no goroutine per stream and no leak when the
  client disconnects, and it maps directly onto reading an upstream body line by line.
- **Typed chunks, not raw bytes.** Gemini and Anthropic have to translate anyway, usage is
  read from the chunks, and re-encoding costs microseconds against a 10 ms budget.
- **The commit point is the first successful chunk, not the call.** The handler pulls one
  chunk before it writes headers, so an adapter that fails on its first read can still fail
  over in step 5.
- **The unknown polymorphic parts of the request (content parts, tools, stop) stay
  `json.RawMessage`.** This keeps fidelity for passthrough without modelling OpenAI's whole
  schema.
- **The error envelope is a small copy in `gateway`.** It is OpenAI's fixed four-field shape,
  so it cannot drift in any way that matters.
- **New config: `V1_REQUEST_TIMEOUT`, default 5m.** It is validated in `config`, as for every
  other duration.
- **`/v1` is mounted only when `APP_ENV=development` until step 3.** Without gateway keys it
  would be an open endpoint in production.

## Rejected candidate

The spec-literal shape: `Chat` + `ChatStream` + `Embed`, a stream delivered as a
`<-chan Event`, and raw upstream bytes passed through. It has more surface per adapter, a
goroutine and cancellation rules per stream, and it cannot translate non-OpenAI providers or
read usage.

## Out of scope

Gateway keys and auth (step 3), routes and alias resolution (step 3), real adapters (step 4),
rate limiting and fallback (step 5), usage events (step 6), and aggregating tool-call deltas
in `Assemble` (step 4, when a real adapter can emit them).

## Tests

- `provider/mock`: each model's behaviour, and that Next returns once ctx is cancelled.
- `provider`: Assemble over a scripted stream.
- `gateway` (httptest): the SSE frames and `[DONE]`; the non-stream JSON; 429 with
  Retry-After; 413; 502; `mock/break` sending 3 chunks and then an error event; a cancelled
  client stopping the mock; a missing model or messages returning 400; an oversized body
  returning 413.
- Manual: the official OpenAI Python SDK pointed at `base_url=http://localhost:8080/v1`.
