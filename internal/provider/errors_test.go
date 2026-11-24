package provider

import "testing"

func TestContextTooLongReadsTheStatedNumbers(t *testing.T) {
	for _, tc := range []struct {
		vendor, message   string
		window, requested int
	}{
		{"openai", "This model's maximum context length is 8192 tokens. However, your messages resulted in 9013 tokens. Please reduce the length of the messages.", 8192, 9013},
		{"openai, completion included", "This model's maximum context length is 8192 tokens. However, you requested 8910 tokens (10 in the messages, 8900 in the completion). Please reduce the length of the messages or completion.", 8192, 10},
		{"openrouter", "This endpoint's maximum context length is 131072 tokens. However, you requested about 140211 tokens (140211 of text input).", 131072, 140211},
		{"groq", "Please reduce the length of the messages or completion. The maximum context length is 8192 tokens.", 8192, 0},
		{"anthropic", "prompt is too long: 210000 tokens > 200000 maximum", 200000, 210000},
		{"mistral", "Prompt contains 40000 tokens and 0 draft tokens, too large for model with 32768 maximum context length", 32768, 40000},
		{"gemini", "The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).", 1048576, 1200000},
		{"cerebras", "Please reduce the length of the messages or completion. Current length is 9000 while limit is 8192", 8192, 9000},
		{"unstated", "Please reduce the length of the messages or completion.", 0, 0},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			e := ContextTooLong(tc.message, "detail")
			if e.Kind != ErrContextTooLong || e.Window != tc.window || e.Requested != tc.requested {
				t.Errorf("got kind %v, window %d, requested %d; want window %d, requested %d",
					e.Kind, e.Window, e.Requested, tc.window, tc.requested)
			}
			if w, r := ContextLimit(e); w != tc.window || r != tc.requested {
				t.Errorf("ContextLimit = %d, %d", w, r)
			}
		})
	}
}
