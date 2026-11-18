package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestResetPasswordPageIsServedUnderACSPThatCoversItsInlineCode(t *testing.T) {
	mux := http.NewServeMux()
	ResetPasswordPage(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reset-password?token=abc", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for name, want := range map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "connect-src 'self'", "form-action 'none'", "base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP %q lacks %q", csp, directive)
		}
	}

	body := rec.Body.String()
	for _, tag := range []string{"script", "style"} {
		blocks := regexp.MustCompile(`(?s)<`+tag+`>(.*?)</`+tag+`>`).FindAllStringSubmatch(body, -1)
		if len(blocks) == 0 {
			t.Fatalf("no inline <%s> in the page", tag)
		}
		for _, block := range blocks {
			sum := sha256.Sum256([]byte(block[1]))
			hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !strings.Contains(csp, hash) {
				t.Errorf("inline <%s> hashes to %s, which the CSP does not list:\n%s", tag, hash, csp)
			}
		}
	}
	if strings.Contains(body, "<script src") || strings.Contains(body, "<link") {
		t.Error("page loads an external resource")
	}
}
