package provider

import (
	"encoding/json"
	"testing"
)

func TestMessageTextReadsStringsAndTextParts(t *testing.T) {
	cases := map[string]string{
		`"plain"`: "plain",
		`[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"b"}]`: "ab",
		`null`: "",
		`42`:   "",
	}
	for raw, want := range cases {
		if got := (Message{Content: json.RawMessage(raw)}).Text(); got != want {
			t.Errorf("Text(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestAssemblerJoinsDeltasPerChoice(t *testing.T) {
	stop := "stop"
	var a Assembler
	a.Add(&ChatChunk{ID: "c1", Model: "m", Created: 5, Choices: []ChunkChoice{{Delta: Delta{Role: "assistant", Content: "Hel"}}}})
	a.Add(&ChatChunk{ID: "c1", Choices: []ChunkChoice{{Delta: Delta{Content: "lo"}}, {Index: 1, Delta: Delta{Content: "second"}}}})
	a.Add(&ChatChunk{ID: "c1", Choices: []ChunkChoice{{FinishReason: &stop}, {Index: -1}, {Index: 1 << 30}}})
	a.Add(&ChatChunk{ID: "c1", Choices: []ChunkChoice{}, Usage: &Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}})

	r := a.Response()
	if r.ID != "c1" || r.Model != "m" || r.Created != 5 || r.Object != "chat.completion" {
		t.Errorf("header = %+v", r)
	}
	if len(r.Choices) != 2 {
		t.Fatalf("%d choices, want 2 (out-of-range indexes dropped)", len(r.Choices))
	}
	if c := r.Choices[0]; c.Message.Content != "Hello" || c.Message.Role != "assistant" || c.FinishReason == nil || *c.FinishReason != "stop" {
		t.Errorf("choice 0 = %+v", c)
	}
	if c := r.Choices[1]; c.Message.Content != "second" || c.Index != 1 {
		t.Errorf("choice 1 = %+v", c)
	}
	if r.Usage == nil || r.Usage.TotalTokens != 5 {
		t.Errorf("usage = %+v", r.Usage)
	}
}
