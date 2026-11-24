package promptlog

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEachRuleRedactsItsSecrets(t *testing.T) {
	cases := []struct{ rule, name, in, secret string }{
		{"bearer token", "header", `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig`, "eyJhbGciOiJIUzI1NiJ9"},
		{"bearer token", "curl", `-H "authorization: bearer abcdefghijklmnopqrstuvwxyz"`, "abcdefghijklmnopqrstuvwxyz"},
		{"prefixed key", "openai", `use sk-proj-AbCdEfGhIjKlMnOpQrStUv`, "AbCdEfGhIjKlMnOpQrStUv"},
		{"prefixed key", "anthropic", `"sk-ant-api03-AbCdEfGhIjKlMnOpQrStUv"`, "AbCdEfGhIjKlMnOpQrStUv"},
		{"prefixed key", "gateway key", `gwk_AbCdEfGhIjKlMnOpQrStUvWxYz012345678901234`, "AbCdEfGhIjKlMnOpQrStUv"},
		{"prefixed key", "google", `key=AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz0123456`, "SyAbCdEfGhIjKlMnOpQrSt"},
		{"prefixed key", "groq", `gsk_AbCdEfGhIjKlMnOpQrStUvWxYz`, "AbCdEfGhIjKlMnOpQrStUv"},
		{"prefixed key", "cerebras", `csk-abcdefghijklmnopqrstuvwxyz`, "abcdefghijklmnopqrstuv"},
		{"prefixed key", "aws access key", `AKIAIOSFODNN7EXAMPLE`, "IOSFODNN7EXAMPLE"},
		{"prefixed key", "slack bot", `xoxb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx`, "AbCdEfGhIjKlMnOpQrStUvWx"},
		{"prefixed key", "slack user", `xoxp-123456789012-1234567890123-AbCdEfGh`, "1234567890123-AbCdEfGh"},
		{"prefixed key", "gitlab", `glpat-AbCdEfGhIjKlMnOpQrSt`, "AbCdEfGhIjKlMnOpQrSt"},
		{"prefixed key", "github oauth", `gho_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789`, "AbCdEfGhIjKlMnOpQrSt"},
		{"prefixed key", "github server", `ghs_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789`, "AbCdEfGhIjKlMnOpQrSt"},
		{"prefixed key", "github user", `ghu_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789`, "AbCdEfGhIjKlMnOpQrSt"},
		{"prefixed key", "github refresh", `ghr_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789`, "AbCdEfGhIjKlMnOpQrSt"},
		{"prefixed key", "stripe restricted", `rk_live_AbCdEfGhIjKlMnOpQrStUv`, "AbCdEfGhIjKlMnOpQrStUv"},
		{"prefixed key", "google oauth", `ya29.a0AfH6SMBxAbCdEfGhIjKlMnOpQrStUvWx`, "a0AfH6SMBxAbCdEfGhIj"},
		{"prefixed key", "nvidia", `nvapi-AbCdEfGhIjKlMnOpQrStUvWxYz012345`, "AbCdEfGhIjKlMnOpQrSt"},
		{"jwt", "bare", `token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnop here`, "eyJzdWIiOiIxMjM0NTY3ODkwIn0"},
		{"pem private key", "block", `-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA1234567890abcdef\n-----END RSA PRIVATE KEY-----`, "MIIEpAIBAAKCAQEA"},
		{"pem private key", "unterminated", `"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC`, "MIIEvQIBADANBgkq"},
		{"url credentials", "postgres", `postgres://admin:hunter2hunter2@db.example.com/prod`, "hunter2hunter2"},
		{"url credentials", "https", `https://bot:p4ss@example.com`, "p4ss"},
		{"named value", "json api_key", `{"api_key": "plainvalue123"}`, "plainvalue123"},
		{"named value", "json password", `{"user":"a","password":"hunter2hunter2"}`, "hunter2hunter2"},
		{"named value", "json clientSecret", `{"clientSecret":"s3cr3t-value"}`, "s3cr3t-value"},
		{"named value", "json in a string", `{"content":"{\"api_key\": \"plainvalue123\"}"}`, "plainvalue123"},
		{"named value", "text", `connect with password=correcthorse and go`, "correcthorse"},
		{"named value", "escaped quote in value", `"api_key": "\"quoted-escapes-abcdefgh"`, "quoted-escapes"},
		{"named value", "aws secret", `aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`, "wJalrXUtnFEMI"},
		{"named value", "django", `SECRET_KEY = 'django-insecure-abcdefghijklmnop'`, "django-insecure"},
		{"named value", "json secret_key", `{"secret_key":"abcdefghijklmnopqrst"}`, "abcdefghijklmnopqrst"},
		{"named value", "json private_key", `{"private_key":"abcdefghijklmnopqrst"}`, "abcdefghijklmnopqrst"},
		{"named value", "json token", `{"token":"abcdefghijklmnopqrstuv"}`, "abcdefghijklmnopqrstuv"},
		{"named value", "basic auth", `Authorization: Basic dXNlcjpwYXNzd29yZDEyMzQ1Ng==`, "dXNlcjpwYXNzd29yZDEyMzQ1Ng"},
	}
	for _, c := range cases {
		got := redact(c.in, []rule{ruleNamed(t, c.rule)})
		if strings.Contains(got, c.secret) || !strings.Contains(got, redacted) {
			t.Errorf("%s/%s: %q became %q", c.rule, c.name, c.in, got)
		}
	}
}

func ruleNamed(t *testing.T, name string) rule {
	t.Helper()
	for _, r := range rules {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("no rule named %q", name)
	return rule{}
}

func TestPrepareRedactsSecretsQuotedInsideAJSONString(t *testing.T) {
	for _, content := range []string{
		`MISTRAL_API_KEY="AbCdEfGhIjKlMnOpQrStUvWx"`,
		`password: "hunter2hunter2"`,
		`api_key: 'AbCdEfGhIjKlMnOpQrStUvWx'`,
		`export STRIPE_SECRET='zzzzzzzzzzzzzzzzzzz'`,
		`MISTRAL_API_KEY=AbCdEfGhIjKlMnOpQrStUvWx`,
	} {
		body, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": content}}})
		got, _ := Prepare(body)
		if strings.Contains(got, "AbCdEfGh") || strings.Contains(got, "hunter2") || strings.Contains(got, "zzzzzzzz") {
			t.Errorf("%s: stored %s", content, got)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("%s: stored invalid JSON %s", content, got)
		}
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	for _, in := range []string{
		`{"model":"fast","max_tokens":256,"messages":[{"role":"user","content":"ask about skiing"}]}`,
		`{"usage":{"prompt_tokens":123456789,"completion_tokens":123456789}}`,
		`{"password": "short"}`,
		`a task-list of things to do`,
		`the tokenizer: cl100k_base_encoding`,
		`mail me at someone@example.com or see https://example.com/a@b`,
		`a basic understanding of bearer bonds`,
	} {
		if got := redact(in, rules); got != in {
			t.Errorf("redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestRedactKeepsJSONValid(t *testing.T) {
	in := `{"api_key":"plainvalue123","n":1}`
	if got, want := redact(in, rules), `{"api_key":"[REDACTED]","n":1}`; got != want {
		t.Errorf("redact = %s, want %s", got, want)
	}
}

func TestPrepareLeavesASmallBodyWhole(t *testing.T) {
	got, cut := Prepare([]byte(`{"a":"b"}`))
	if got != `{"a":"b"}` || cut {
		t.Errorf("Prepare = %q, %v", got, cut)
	}
	if got, cut := Prepare(nil); got != "" || cut {
		t.Errorf("Prepare(nil) = %q, %v", got, cut)
	}
}

func TestPrepareStoresInvalidUTF8AsValid(t *testing.T) {
	got, cut := Prepare([]byte("{\"content\":\"caf\xe9 \xff\xfe\"}"))
	if cut || !utf8.ValidString(got) || !strings.Contains(got, "caf�") {
		t.Errorf("Prepare = %q, %v", got, cut)
	}
}

func TestPrepareCapsOnARuneBoundary(t *testing.T) {
	body := strings.Repeat("é", MaxBodyBytes)
	got, cut := Prepare([]byte(body))
	if !cut || len(got) > MaxBodyBytes || !utf8.ValidString(got) {
		t.Errorf("cut=%v len=%d valid=%v", cut, len(got), utf8.ValidString(got))
	}
}

func TestPrepareDropsASecretTheCapSplit(t *testing.T) {
	for _, c := range []struct{ name, secret, stub string }{
		{"prefixed key", "sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz", "sk-pro"},
		{"named value", `"password":"Tr0ub4dor&3correcthorse"`, "Tr0ub4dor"},
	} {
		at := MaxBodyBytes - len(c.stub) - strings.Index(c.secret, c.stub)
		body := strings.Repeat(" ", at) + c.secret + " tail"
		got, cut := Prepare([]byte(body))
		if !cut || strings.Contains(got, c.stub) || len(got) > MaxBodyBytes {
			t.Errorf("%s: cut=%v, tail %q", c.name, cut, got[max(0, len(got)-30):])
		}
	}
}

func TestPrepareStaysUnderTheCapAfterRedaction(t *testing.T) {
	body := strings.Repeat(`"password":"12345678",`, MaxBodyBytes/22)
	got, cut := Prepare([]byte(body))
	if len(got) > MaxBodyBytes || !cut || strings.Contains(got, "12345678") {
		t.Errorf("len=%d cut=%v", len(got), cut)
	}
}

func BenchmarkPrepare(b *testing.B) {
	prose := strings.Repeat("hello world the quick brown fox ", MaxBodyBytes/32)
	for _, c := range []struct{ name, body string }{
		{"prose", `{"model":"x","messages":[{"role":"user","content":"` + prose + `"}]}`},
		{"alnum", strings.Repeat("abcdefghij0123456789", MaxBodyBytes/20)},
		{"secret words", strings.Repeat(`count the tokens, keep the secret: shh, password=hunter2hunter2 `, MaxBodyBytes/64)},
	} {
		b.Run(c.name, func(b *testing.B) {
			body := []byte(c.body)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				Prepare(body)
			}
		})
	}
}
