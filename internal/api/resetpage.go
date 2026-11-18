package api

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"net/http"
	"strings"
)

//go:embed resetpage/index.html resetpage/page.js resetpage/page.css
var resetPageFiles embed.FS

var resetPage = buildResetPage()

type builtPage struct {
	html []byte
	csp  string
}

func buildResetPage() builtPage {
	read := func(name string) string {
		b, err := resetPageFiles.ReadFile("resetpage/" + name)
		if err != nil {
			panic(err)
		}
		// A browser hashes an inline block after HTML parsing has turned CRLF
		// into LF, so a CRLF checkout would otherwise serve a CSP that blocks it.
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	script, style := read("page.js"), read("page.css")
	html := strings.NewReplacer(
		"/*SCRIPT*/", script,
		"/*STYLE*/", style,
	).Replace(read("index.html"))

	hash := func(text string) string {
		sum := sha256.Sum256([]byte(text))
		return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	return builtPage{
		html: []byte(html),
		csp: "default-src 'none'; script-src " + hash(script) + "; style-src " + hash(style) +
			"; connect-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'",
	}
}

// ResetPasswordPage mounts the page the password-reset mail links to.
func ResetPasswordPage(mux *http.ServeMux) {
	mux.HandleFunc("GET /reset-password", func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", resetPage.csp)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(resetPage.html)
	})
}
