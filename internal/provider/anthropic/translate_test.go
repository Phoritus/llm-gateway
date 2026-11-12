package anthropic

import (
	"encoding/json"
	"errors"
	"testing"

	"llm-gateway/internal/provider"
)

func chatRequest(t *testing.T, body string) *provider.ChatRequest {
	t.Helper()
	var req provider.ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return &req
}

// wire translates body and returns the result as generic JSON, so assertions
// are about what Anthropic receives rather than about the Go structs.
func wire(t *testing.T, body string) map[string]any {
	t.Helper()
	out, err := translate(chatRequest(t, body))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	b, _ := json.Marshal(out)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestTranslateHoistsSystemAndDeveloperMessages(t *testing.T) {
	m := wire(t, `{"model":"claude","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hi"},
		{"role":"developer","content":[{"type":"text","text":"use "},{"type":"text","text":"metric"}]}]}`)
	if got := asJSON(m["system"]); got != `[{"text":"be brief","type":"text"},{"text":"use metric","type":"text"}]` {
		t.Errorf("system = %s", got)
	}
	if got := asJSON(m["messages"]); got != `[{"content":[{"text":"hi","type":"text"}],"role":"user"}]` {
		t.Errorf("messages = %s", got)
	}
	if m["stream"] != true || m["model"] != "claude" {
		t.Errorf("model/stream = %v/%v", m["model"], m["stream"])
	}
}

func TestTranslateToolRoundTripMergesParallelResults(t *testing.T) {
	m := wire(t, `{"model":"c","messages":[
		{"role":"user","content":"weather in two cities"},
		{"role":"assistant","content":"checking","tool_calls":[
			{"id":"t1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Oslo\"}"}},
			{"id":"t2","type":"function","function":{"name":"weather","arguments":""}}]},
		{"role":"tool","tool_call_id":"t1","content":"cold"},
		{"role":"tool","tool_call_id":"t2","content":"mild"},
		{"role":"user","content":"thanks"}]}`)
	want := `[` +
		`{"content":[{"text":"weather in two cities","type":"text"}],"role":"user"},` +
		`{"content":[{"text":"checking","type":"text"},{"id":"t1","input":{"city":"Oslo"},"name":"weather","type":"tool_use"},{"id":"t2","input":{},"name":"weather","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"cold","tool_use_id":"t1","type":"tool_result"},{"content":"mild","tool_use_id":"t2","type":"tool_result"},{"text":"thanks","type":"text"}],"role":"user"}]`
	if got := asJSON(m["messages"]); got != want {
		t.Errorf("messages =\n%s\nwant\n%s", got, want)
	}
}

func TestTranslateImages(t *testing.T) {
	m := wire(t, `{"model":"c","messages":[{"role":"user","content":[
		{"type":"text","text":"what is this"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
		{"type":"image_url","image_url":{"url":"https://example.com/a.jpg"}}]}]}`)
	want := `[{"content":[{"text":"what is this","type":"text"},` +
		`{"source":{"data":"AAAA","media_type":"image/png","type":"base64"},"type":"image"},` +
		`{"source":{"type":"url","url":"https://example.com/a.jpg"},"type":"image"}],"role":"user"}]`
	if got := asJSON(m["messages"]); got != want {
		t.Errorf("messages =\n%s\nwant\n%s", got, want)
	}
}

func TestTranslateSamplingAndLimits(t *testing.T) {
	t.Run("temperature and top_p are dropped, max_tokens defaults", func(t *testing.T) {
		m := wire(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"temperature":0.2,"top_p":0.9,"seed":1,"stop":"END"}`)
		for _, k := range []string{"temperature", "top_p", "seed"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s was forwarded", k)
			}
		}
		if m["max_tokens"] != float64(defaultMaxTokens) {
			t.Errorf("max_tokens = %v", m["max_tokens"])
		}
		if asJSON(m["stop_sequences"]) != `["END"]` {
			t.Errorf("stop_sequences = %v", m["stop_sequences"])
		}
	})
	t.Run("stop array", func(t *testing.T) {
		m := wire(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"stop":["a","b"]}`)
		if asJSON(m["stop_sequences"]) != `["a","b"]` {
			t.Errorf("stop_sequences = %v", m["stop_sequences"])
		}
	})
	t.Run("max_completion_tokens", func(t *testing.T) {
		m := wire(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"max_completion_tokens":77}`)
		if m["max_tokens"] != float64(77) {
			t.Errorf("max_tokens = %v", m["max_tokens"])
		}
	})
	t.Run("max_tokens wins", func(t *testing.T) {
		m := wire(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"max_tokens":5,"max_completion_tokens":77}`)
		if m["max_tokens"] != float64(5) {
			t.Errorf("max_tokens = %v", m["max_tokens"])
		}
	})
	t.Run("user becomes metadata", func(t *testing.T) {
		m := wire(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"user":"u-1"}`)
		if asJSON(m["metadata"]) != `{"user_id":"u-1"}` {
			t.Errorf("metadata = %v", m["metadata"])
		}
	})
}

func TestTranslateTools(t *testing.T) {
	const tools = `"tools":[{"type":"function","function":{"name":"a","description":"does a","parameters":{"type":"object","properties":{"x":{"type":"string"}}},"strict":true}},{"type":"function","function":{"name":"b"}}]`
	base := `{"model":"c","messages":[{"role":"user","content":"x"}],` + tools

	m := wire(t, base+`}`)
	want := `[{"description":"does a","input_schema":{"properties":{"x":{"type":"string"}},"type":"object"},"name":"a","strict":true},{"input_schema":{"type":"object"},"name":"b"}]`
	if got := asJSON(m["tools"]); got != want {
		t.Errorf("tools =\n%s\nwant\n%s", got, want)
	}
	if _, ok := m["tool_choice"]; ok {
		t.Errorf("tool_choice = %v with none requested", m["tool_choice"])
	}

	for name, tc := range map[string]struct{ extra, want string }{
		"auto":              {`,"tool_choice":"auto"`, `{"type":"auto"}`},
		"none":              {`,"tool_choice":"none"`, `{"type":"none"}`},
		"required":          {`,"tool_choice":"required"`, `{"type":"any"}`},
		"named":             {`,"tool_choice":{"type":"function","function":{"name":"b"}}`, `{"name":"b","type":"tool"}`},
		"serial only":       {`,"parallel_tool_calls":false`, `{"disable_parallel_tool_use":true,"type":"auto"}`},
		"serial and named":  {`,"parallel_tool_calls":false,"tool_choice":"required"`, `{"disable_parallel_tool_use":true,"type":"any"}`},
		"parallel explicit": {`,"parallel_tool_calls":true,"tool_choice":"auto"`, `{"type":"auto"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := asJSON(wire(t, base+tc.extra+`}`)["tool_choice"]); got != tc.want {
				t.Errorf("tool_choice = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTranslateResponseFormat(t *testing.T) {
	m := wire(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"n","schema":{"type":"object"}}}}`)
	if got := asJSON(m["output_config"]); got != `{"format":{"schema":{"type":"object"},"type":"json_schema"}}` {
		t.Errorf("output_config = %s", got)
	}
	m = wire(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"response_format":{"type":"text"}}`)
	if _, ok := m["output_config"]; ok {
		t.Error("response_format text produced output_config")
	}
}

func TestTranslateRefusals(t *testing.T) {
	user := `{"role":"user","content":"x"}`
	for name, body := range map[string]string{
		"n above one":         `{"model":"c","messages":[` + user + `],"n":2}`,
		"json_object":         `{"model":"c","messages":[` + user + `],"response_format":{"type":"json_object"}}`,
		"audio part":          `{"model":"c","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`,
		"bad arguments":       `{"model":"c","messages":[` + user + `,{"role":"assistant","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{nope"}}]}]}`,
		"arguments array":     `{"model":"c","messages":[` + user + `,{"role":"assistant","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"[1]"}}]}]}`,
		"unknown role":        `{"model":"c","messages":[{"role":"function","content":"x"}]}`,
		"only system":         `{"model":"c","messages":[{"role":"system","content":"x"}]}`,
		"no messages":         `{"model":"c","messages":[]}`,
		"relative image":      `{"model":"c","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"file.png"}}]}]}`,
		"unsupported tool":    `{"model":"c","messages":[` + user + `],"tools":[{"type":"web_search"}]}`,
		"unknown tool_choice": `{"model":"c","messages":[` + user + `],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"sometimes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := translate(chatRequest(t, body))
			if !errors.Is(err, provider.ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
