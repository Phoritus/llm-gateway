package handoff

import (
	_ "embed"
	"encoding/json"

	"llm-gateway/internal/provider"
)

//go:embed windows.json
var windowsJSON []byte

var catalog = func() map[provider.Vendor]map[string]int {
	var c map[provider.Vendor]map[string]int
	if err := json.Unmarshal(windowsJSON, &c); err != nil {
		panic("handoff: windows.json: " + err.Error())
	}
	return c
}()

// Window is the catalogue's context window for a vendor's model. It is a
// starting guess, an upper bound a free tier may undercut: a window learned
// from the vendor's own error always wins over it.
func Window(v provider.Vendor, model string) (int, bool) {
	n, ok := catalog[v][model]
	return n, ok
}
