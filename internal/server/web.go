package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// webFS embeds the built frontend. The build copies web/dist into
// internal/server/web/dist before compiling; a placeholder index keeps
// plain `go build ./...` working on a fresh clone.
//
//go:embed all:web/dist
var webFS embed.FS

func webIndex(w http.ResponseWriter) {
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func webServe(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
		return
	}
	webIndex(w)
}
