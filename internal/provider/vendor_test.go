package provider

import "testing"

func TestParseVendor(t *testing.T) {
	for _, v := range Vendors() {
		got, ok := ParseVendor(string(v))
		if !ok || got != v {
			t.Errorf("ParseVendor(%q) = %q, %v", v, got, ok)
		}
	}
	for _, s := range []string{"", "OpenAI", "ANTHROPIC", " openai", "Mock", "unknown"} {
		if v, ok := ParseVendor(s); ok {
			t.Errorf("ParseVendor(%q) = %q, want no match", s, v)
		}
	}
}
