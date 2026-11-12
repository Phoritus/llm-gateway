package provider

import (
	"encoding/json"
	"strings"
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

func TestAssemblerMergesToolCallDeltas(t *testing.T) {
	tc := "tool_calls"
	var a Assembler
	a.Add(&ChatChunk{ID: "c", Choices: []ChunkChoice{{Delta: Delta{Role: "assistant",
		ToolCalls: json.RawMessage(`[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""}}]`)}}}})
	a.Add(&ChatChunk{ID: "c", Choices: []ChunkChoice{{Delta: Delta{
		ToolCalls: json.RawMessage(`[{"index":0,"function":{"arguments":"{\"city\":"}},{"index":1,"id":"call_b","function":{"name":"now","arguments":"{}"}}]`)}}}})
	a.Add(&ChatChunk{ID: "c", Choices: []ChunkChoice{{Delta: Delta{
		ToolCalls: json.RawMessage(`[{"index":0,"function":{"arguments":"\"Oslo\"}"}},{"index":500},{"index":-1}]`)}}}})
	a.Add(&ChatChunk{ID: "c", Choices: []ChunkChoice{{Delta: Delta{ToolCalls: json.RawMessage(`"garbage"`)}, FinishReason: &tc}}})

	msg := a.Response().Choices[0].Message
	want := []ToolCall{
		{ID: "call_a", Type: "function", Function: ToolFunction{Name: "get_weather", Arguments: `{"city":"Oslo"}`}},
		{ID: "call_b", Type: "function", Function: ToolFunction{Name: "now", Arguments: `{}`}},
	}
	if len(msg.ToolCalls) != len(want) {
		t.Fatalf("tool calls = %+v, want %+v", msg.ToolCalls, want)
	}
	for i := range want {
		if msg.ToolCalls[i] != want[i] {
			t.Errorf("tool call %d = %+v, want %+v", i, msg.ToolCalls[i], want[i])
		}
	}
	b, _ := json.Marshal(msg)
	if !strings.Contains(string(b), `"tool_calls":[`) {
		t.Errorf("message JSON lacks tool_calls: %s", b)
	}
}

func TestChatRequestKeepsFieldsItDoesNotModel(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2,"seed":7,` +
		`"response_format":{"type":"json_object"},"frequency_penalty":0.5,"MODEL":"shadow"}`
	var req ChatRequest
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.Extra["MODEL"]; ok {
		t.Error("a case variant of a modelled field was kept as an extra")
	}
	req.Model = "upstream-model"
	req.Stream = true
	out, err := json.Marshal(&req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"model": `"upstream-model"`, "stream": `true`, "n": `2`, "seed": `7`,
		"response_format": `{"type":"json_object"}`, "frequency_penalty": `0.5`,
	} {
		if string(got[key]) != want {
			t.Errorf("%s = %s, want %s (body %s)", key, got[key], want, out)
		}
	}

	req.Extra = map[string]json.RawMessage{"model": json.RawMessage(`"from-extra"`)}
	out, _ = json.Marshal(req)
	if !strings.Contains(string(out), `"model":"upstream-model"`) {
		t.Errorf("an extra overrode a modelled field: %s", out)
	}
}
