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

// resetPage is rendered once. The CSP names the hash of each inlined block, so
// the page runs with no external resource and no 'unsafe-inline'.
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
		return string(b)
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

// ResetPasswordPage mounts the page the password-reset mail links to, at the
// server root because it is a browser page and not part of the /api surface.
// The token arrives in the query string, hence no-referrer and no-store.
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
