package promptlog

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"
)

// MaxBodyBytes caps each stored side of a log.
const MaxBodyBytes = 32 << 10

const (
	redacted = "[REDACTED]"
	// stubLookback bounds how far back from a cut Prepare looks for the
	// boundary that ends the last whole token.
	stubLookback = 512
	minValue     = 8
)

// rule finds one shape of secret in a text and marks the bytes to replace.
type rule struct {
	name string
	find func(t *text)
}

var rules = []rule{
	{"bearer token", findBearerTokens},
	{"prefixed key", findPrefixedKeys},
	{"jwt", findJWTs},
	{"pem private key", findPrivateKeyBlocks},
	{"url credentials", findURLCredentials},
	{"named value", findNamedValues},
}

type keyPrefix struct {
	prefix string
	char   func(byte) bool
}

var keyPrefixes = map[string]keyPrefix{
	"openai or anthropic":    {"sk-", isKeyChar},
	"stripe live":            {"sk_live_", isKeyChar},
	"stripe test":            {"sk_test_", isKeyChar},
	"stripe restricted":      {"rk_live_", isKeyChar},
	"stripe restricted test": {"rk_test_", isKeyChar},
	"gateway key":            {"gwk_", isKeyChar},
	"groq":                   {"gsk_", isKeyChar},
	"cerebras":               {"csk-", isKeyChar},
	"xai":                    {"xai-", isKeyChar},
	"google api key":         {"AIza", isKeyChar},
	"google oauth":           {"ya29.", isKeyChar},
	"github classic":         {"ghp_", isKeyChar},
	"github oauth":           {"gho_", isKeyChar},
	"github server":          {"ghs_", isKeyChar},
	"github user":            {"ghu_", isKeyChar},
	"github refresh":         {"ghr_", isKeyChar},
	"github fine-grained":    {"github_pat_", isKeyChar},
	"hugging face":           {"hf_", isKeyChar},
	"gitlab":                 {"glpat-", isKeyChar},
	"slack bot":              {"xoxb-", isKeyChar},
	"slack user":             {"xoxp-", isKeyChar},
	"nvidia":                 {"nvapi-", isKeyChar},
	"aws access key":         {"AKIA", isUpperAlnum},
	"aws temporary key":      {"ASIA", isUpperAlnum},
}

const minKeyTail = 16

// secretWords make a field or variable a secret when its name holds one as a
// whole word: aws_secret_access_key, clientSecret, x-api-key, but not
// max_tokens or tokenizer.
var secretWords = []string{
	"password", "passwd", "secret", "token", "api_key", "api-key", "apikey",
	"private_key", "private-key", "privatekey", "authorization",
}

// authSchemes are the words an Authorization value starts with before its
// credential.
var authSchemes = []string{"basic", "bearer", "digest"}

type span struct{ from, to int }

type text struct {
	s string
	// lower is s with ASCII letters lowered, byte for byte, so an index into
	// one is an index into the other.
	lower string
	marks []span
}

func (t *text) mark(from, to int) {
	if to > from {
		t.marks = append(t.marks, span{from, to})
	}
}

// Prepare turns a body into what is stored: valid UTF-8, cut to MaxBodyBytes
// on a UTF-8 boundary, then with its secrets redacted. It reports whether the
// body was cut.
func Prepare(raw []byte) (string, bool) {
	s := strings.ToValidUTF8(string(raw[:min(len(raw), MaxBodyBytes+1)]), "�")
	cut := len(raw) > MaxBodyBytes || len(s) > MaxBodyBytes
	if cut {
		s = dropSplitSecretStub(s[:runeBoundary(s, MaxBodyBytes)])
	}
	s = redact(s, rules)
	if len(s) > MaxBodyBytes {
		s, cut = s[:runeBoundary(s, MaxBodyBytes)], true
	}
	return s, cut
}

// dropSplitSecretStub ends a cut body at the last boundary before the cut, so
// a secret the cut split leaves no stub too short for its rule to know.
func dropSplitSecretStub(s string) string {
	floor := max(0, len(s)-stubLookback)
	if i := strings.LastIndexAny(s[floor:], " \t\r\n\"',"); i >= 0 {
		return s[:floor+i+1]
	}
	return s[:runeBoundary(s, floor)]
}

func redact(s string, rs []rule) string {
	t := &text{s: s, lower: asciiLower(s)}
	for _, r := range rs {
		r.find(t)
	}
	if len(t.marks) == 0 {
		return s
	}
	slices.SortFunc(t.marks, func(a, b span) int { return cmp.Compare(a.from, b.from) })
	var b strings.Builder
	b.Grow(len(s))
	at := 0
	for i := 0; i < len(t.marks); {
		from, to := t.marks[i].from, t.marks[i].to
		for i++; i < len(t.marks) && t.marks[i].from <= to; i++ {
			to = max(to, t.marks[i].to)
		}
		b.WriteString(s[at:from])
		b.WriteString(redacted)
		at = to
	}
	b.WriteString(s[at:])
	return b.String()
}

// each calls f with the index of every occurrence of sub in s.
func each(s, sub string, f func(i int)) {
	for at := 0; ; {
		i := strings.Index(s[at:], sub)
		if i < 0 {
			return
		}
		f(at + i)
		at += i + len(sub)
	}
}

func findBearerTokens(t *text) {
	each(t.lower, "bearer", func(i int) {
		if i > 0 && isAlnum(t.s[i-1]) {
			return
		}
		from := skipSpace(t.s, i+len("bearer"))
		if from == i+len("bearer") {
			return
		}
		to := scan(t.s, from, isBearerChar)
		if to-from < minKeyTail {
			return
		}
		for to < len(t.s) && t.s[to] == '=' {
			to++
		}
		t.mark(from, to)
	})
}

func findPrefixedKeys(t *text) {
	for _, k := range keyPrefixes {
		each(t.s, k.prefix, func(i int) {
			if i > 0 && isWordChar(t.s[i-1]) {
				return
			}
			to := scan(t.s, i+len(k.prefix), k.char)
			if to-i-len(k.prefix) >= minKeyTail {
				t.mark(i, to)
			}
		})
	}
}

func findJWTs(t *text) {
	each(t.s, "eyJ", func(i int) {
		if i > 0 && isKeyChar(t.s[i-1]) {
			return
		}
		header := scan(t.s, i, isKeyChar)
		if header >= len(t.s) || t.s[header] != '.' || !strings.HasPrefix(t.s[header+1:], "eyJ") {
			return
		}
		payload := scan(t.s, header+1, isKeyChar)
		if payload >= len(t.s) || t.s[payload] != '.' {
			return
		}
		t.mark(i, scan(t.s, payload+1, isKeyChar))
	})
}

func findPrivateKeyBlocks(t *text) {
	each(t.s, "-----BEGIN ", func(i int) {
		head := strings.Index(t.s[i+len("-----BEGIN "):], "-----")
		if head < 0 || !strings.Contains(t.s[i:i+len("-----BEGIN ")+head], "PRIVATE KEY") {
			return
		}
		rest := i + len("-----BEGIN ") + head + len("-----")
		end := strings.Index(t.s[rest:], "-----END ")
		if end < 0 {
			t.mark(i, len(t.s))
			return
		}
		after := rest + end + len("-----END ")
		end = strings.Index(t.s[after:], "-----")
		if end < 0 {
			t.mark(i, len(t.s))
			return
		}
		t.mark(i, after+end+len("-----"))
	})
}

const maxUserinfo = 256

func findURLCredentials(t *text) {
	each(t.s, "://", func(i int) {
		from := i + len("://")
		colon := -1
		for j := from; j < len(t.s) && j-from <= maxUserinfo; j++ {
			switch c := t.s[j]; {
			case c == '@':
				if colon >= 0 {
					t.mark(colon+1, j)
				}
				return
			case c == ':' && colon < 0:
				colon = j
			case strings.IndexByte("/?# \t\r\n\"'\\<>", c) >= 0:
				return
			}
		}
	})
}

func findNamedValues(t *text) {
	for _, word := range secretWords {
		each(t.lower, word, func(i int) {
			after := i + len(word)
			if after < len(t.s) && isLetter(t.lower[after]) &&
				!(isLowerLetter(t.s[after-1]) && isUpperLetter(t.s[after])) {
				return
			}
			j := scan(t.s, after, isKeyChar)
			switch {
			case strings.HasPrefix(t.s[j:], `\"`):
				j += 2
			case j < len(t.s) && (t.s[j] == '"' || t.s[j] == '\''):
				j++
			}
			j = skipSpace(t.s, j)
			if j >= len(t.s) || (t.s[j] != '=' && t.s[j] != ':') {
				return
			}
			from, to := value(t.s, skipSpace(t.s, j+1))
			if to-from >= minValue {
				t.mark(from, to)
			}
		})
	}
}

// value returns where the value starting at i begins and ends, inside its
// quotes when it has them. A quote never closed runs to the end of s.
func value(s string, i int) (int, int) {
	if i >= len(s) {
		return i, i
	}
	switch {
	case strings.HasPrefix(s[i:], `\"`):
		from := i + 2
		for k := from; k < len(s); k++ {
			if s[k] == '\\' && k+1 < len(s) {
				if s[k+1] == '"' {
					return from, k
				}
				k++
			}
		}
		return from, len(s)
	case s[i] == '"':
		from := i + 1
		for k := from; k < len(s); k++ {
			switch s[k] {
			case '\\':
				k++
			case '"':
				return from, k
			}
		}
		return from, len(s)
	case s[i] == '\'':
		if k := strings.IndexByte(s[i+1:], '\''); k >= 0 {
			return i + 1, i + 1 + k
		}
		return i + 1, len(s)
	case s[i] == '{' || s[i] == '[':
		return i, i
	}
	to := scan(s, i, isBareValueChar)
	word := strings.ToLower(s[i:to])
	if slices.Contains(authSchemes, word) {
		if from := skipSpace(s, to); from > to {
			return from, scan(s, from, isBareValueChar)
		}
	}
	return i, to
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if isUpperLetter(c) {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func scan(s string, i int, ok func(byte) bool) int {
	for i < len(s) && ok(s[i]) {
		i++
	}
	return i
}

func skipSpace(s string, i int) int {
	return scan(s, i, func(c byte) bool { return c == ' ' || c == '\t' })
}

func runeBoundary(s string, n int) int {
	if len(s) <= n {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

func isLowerLetter(c byte) bool { return c >= 'a' && c <= 'z' }
func isUpperLetter(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLetter(c byte) bool      { return isLowerLetter(c) || isUpperLetter(c) }
func isAlnum(c byte) bool       { return isLetter(c) || c >= '0' && c <= '9' }
func isUpperAlnum(c byte) bool  { return isUpperLetter(c) || c >= '0' && c <= '9' }
func isWordChar(c byte) bool    { return isAlnum(c) || c == '_' }
func isKeyChar(c byte) bool     { return isWordChar(c) || c == '-' }
func isBearerChar(c byte) bool {
	return isKeyChar(c) || c == '.' || c == '~' || c == '+' || c == '/'
}
func isBareValueChar(c byte) bool {
	return c > ' ' && strings.IndexByte("\"'\\,;&}])<>", c) < 0
}
