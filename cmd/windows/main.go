// Command windows rebuilds internal/handoff/windows.json, the context window
// catalogue, from OpenRouter's model list and LiteLLM's model data (MIT). It is
// run by hand (make windows) and its output committed: the gateway never calls
// either source at runtime.
//
// Both sources publish upper bounds. A vendor's free tier may serve a smaller
// window, which is why a window learned from the vendor's own error wins.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"llm-gateway/internal/provider"
)

const (
	openRouterURL = "https://openrouter.ai/api/v1/models"
	liteLLMURL    = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
)

func main() {
	out := flag.String("o", "internal/handoff/windows.json", "file to write")
	flag.Parse()

	catalog := map[provider.Vendor]map[string]int{}
	put := func(v provider.Vendor, model string, window int) {
		if window <= 0 || model == "" {
			return
		}
		if catalog[v] == nil {
			catalog[v] = map[string]int{}
		}
		catalog[v][model] = window
	}

	var lite map[string]struct {
		Provider string `json:"litellm_provider"`
		Mode     string `json:"mode"`
		// The file opens with a sample entry whose numbers are strings.
		MaxInputTokens any `json:"max_input_tokens"`
	}
	fetch(liteLLMURL, &lite)
	for key, m := range lite {
		v, ok := provider.ParseVendor(m.Provider)
		if !ok || v.DevelopmentOnly() || m.Mode != "chat" {
			continue
		}
		if n, ok := m.MaxInputTokens.(float64); ok {
			put(v, strings.TrimPrefix(key, m.Provider+"/"), int(n))
		}
	}

	var router struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	fetch(openRouterURL, &router)
	for _, m := range router.Data {
		put(provider.VendorOpenRouter, m.ID, m.ContextLength)
	}

	b, err := json.MarshalIndent(catalog, "", " ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	for v, models := range catalog {
		fmt.Printf("%-10s %d models\n", v, len(models))
	}
}

func fetch(url string, into any) {
	client := http.Client{Timeout: time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		log.Fatalf("fetch %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("fetch %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("read %s: %v", url, err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		log.Fatalf("decode %s: %v", url, err)
	}
}
