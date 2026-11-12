package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"llm-gateway/internal/provider"
)

// errDeadline marks the request's own deadline passing, as opposed to the
// client going away. The first is an upstream that took too long and is
// answered; the second has nobody left to answer.
var errDeadline = errors.New("request deadline exceeded")

// handleChatCompletions is POST /v1/chat/completions.
//
// The point of no return is the first chunk, not the call. The upstream's
// first chunk is read before any header is written, so an upstream that
// accepts the request and then fails at once is still answered with a status
// -- and, once routing lands, can still fail over. After that byte, a failure
// can only end the stream with an error event.
func (d Deps) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeChatRequest(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeoutCause(r.Context(), d.Timeout, errDeadline)
	defer cancel()
	r = r.WithContext(ctx)

	upstream := *req
	if !req.Stream {
		// A non-streaming answer reports usage, so ask the stream for it.
		upstream.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
	}

	stream, err := d.Provider.ChatStream(ctx, &upstream, provider.Credential{})
	if err != nil {
		d.failBeforeFirstByte(w, r, err)
		return
	}
	defer stream.Close()

	first, err := stream.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = &provider.Error{Kind: provider.ErrUpstream, Detail: "stream ended before its first chunk"}
		}
		d.failBeforeFirstByte(w, r, err)
		return
	}

	if req.Stream {
		d.streamResponse(w, r, stream, first)
		return
	}

	var a provider.Assembler
	a.Add(first)
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Nothing has been written yet, so this is still a status.
			d.failBeforeFirstByte(w, r, err)
			return
		}
		a.Add(chunk)
	}
	writeJSON(w, http.StatusOK, a.Response())
}

// streamResponse sends the chunks as server-sent events, flushing each one.
func (d Deps) streamResponse(w http.ResponseWriter, r *http.Request, stream provider.Stream, first *provider.ChatChunk) {
	ctx := r.Context()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Stops a buffering reverse proxy from holding chunks back.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	send := func(payload []byte) bool {
		if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	sendChunk := func(c *provider.ChatChunk) bool {
		b, err := json.Marshal(c)
		return err == nil && send(b)
	}

	if !sendChunk(first) {
		return
	}
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			send([]byte("[DONE]"))
			return
		}
		if err != nil {
			if clientGone(ctx) {
				return
			}
			err = withDeadline(ctx, err)
			d.logUpstream(r, err)
			_, detail := upstreamError(err)
			b, _ := json.Marshal(errorBody{Error: detail})
			send(b)
			return
		}
		if !sendChunk(chunk) {
			return
		}
	}
}

// failBeforeFirstByte answers an upstream failure while a status line can
// still be sent. A client that has gone away gets nothing: there is no one to
// read it.
func (d Deps) failBeforeFirstByte(w http.ResponseWriter, r *http.Request, err error) {
	if clientGone(r.Context()) {
		return
	}
	d.writeUpstreamError(w, r, withDeadline(r.Context(), err))
}

// clientGone reports that the request was cancelled for a reason other than
// its own deadline -- the client disconnected.
func clientGone(ctx context.Context) bool {
	return ctx.Err() != nil && !errors.Is(context.Cause(ctx), errDeadline)
}

// withDeadline replaces a bare context error with errDeadline when the
// request's own deadline is what ended it, so it is answered as a timeout.
func withDeadline(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errDeadline) && errors.Is(err, context.DeadlineExceeded) {
		return errDeadline
	}
	return err
}

// decodeChatRequest reads and validates the body. Unknown fields are
// accepted: OpenAI clients send many this gateway has no use for, and
// refusing them would break every SDK.
func decodeChatRequest(w http.ResponseWriter, r *http.Request) (*provider.ChatRequest, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, typeInvalidRequest, "body_too_large",
				"The request body is too large.")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, typeInvalidRequest, "invalid_body",
			"The request body could not be read.")
		return nil, false
	}

	var req provider.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, typeInvalidRequest, "invalid_json",
			"The request body must be a JSON object in the chat completions format.")
		return nil, false
	}
	if req.Model == "" {
		invalidField(w, "model", "model is required.")
		return nil, false
	}
	if len(req.Messages) == 0 {
		invalidField(w, "messages", "messages must contain at least one message.")
		return nil, false
	}
	for _, m := range req.Messages {
		if m.Role == "" {
			invalidField(w, "messages", "Every message needs a role.")
			return nil, false
		}
	}
	return &req, true
}
