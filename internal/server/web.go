package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// webFS embeds the built frontend. The build copies web/dist into
// internal/server/web/dist before compiling; a placeholder index keeps
// plain `go build ./...` working on a fresh clone.
//
//go:embed all:web/dist
var webFS embed.FS

func webIndex(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
