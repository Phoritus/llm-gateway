package provider

import "context"

// Embedder is an upstream embeddings API. It is separate from Provider because
// not every vendor has one: Anthropic does not.
type Embedder interface {
	Embed(ctx context.Context, req *EmbedRequest, cred Credential) (*EmbedResponse, error)
}

// EmbedRequest embeds one text. Dimensions is left out of the wire request
// when zero, which asks for the model's native size.
type EmbedRequest struct {
	Model      string `json:"model"`
	Input      string `json:"input"`
	Dimensions int    `json:"dimensions,omitempty"`
}

// EmbedResponse is the vector as the upstream returned it, not normalised.
type EmbedResponse struct {
	Vector       []float32
	PromptTokens int
}
