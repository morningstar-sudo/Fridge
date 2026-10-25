// Package web serves the admin UI: static files embedded in the binary, no build step.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var content embed.FS

// Handler serves the embedded UI: "/" is index.html, unknown paths are 404.
// index.html is revalidated every time; vendored files may be cached for a day.
func Handler() http.Handler {
	sub, err := fs.Sub(content, "static")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" || r.URL.Path == "/index.html":
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasPrefix(r.URL.Path, "/vendor/"):
			w.Header().Set("Cache-Control", "public, max-age=86400")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}
		if r.URL.Path == "/index.html" { // FileServer would redirect to "/"; serve it in place
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}
