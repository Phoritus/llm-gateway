package handoff

import (
	"testing"

	"llm-gateway/internal/provider"
)

func TestTheCatalogueCoversTheFreeTierModels(t *testing.T) {
	for _, m := range []struct {
		vendor provider.Vendor
		model  string
	}{
		{provider.VendorGroq, "openai/gpt-oss-120b"},
		{provider.VendorGemini, "gemini-3.5-flash-lite"},
		{provider.VendorOpenRouter, "google/gemma-4-31b-it:free"},
	} {
		if n, ok := Window(m.vendor, m.model); !ok || n < 1000 {
			t.Errorf("Window(%s, %s) = %d, %v", m.vendor, m.model, n, ok)
		}
	}
	if _, ok := Window(provider.VendorMock, "window-8192"); ok {
		t.Error("the mock has a catalogue entry: its windows must be learned")
	}
}
