package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/modules"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.q.ListNodes(r.Context())
	if err != nil {
		handleErr(w, err)
		return
	}
	approved := 0
	for _, n := range nodes {
		if n.Status != "pending" {
			approved++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.version,
		"commit":         s.commit,
		"time":           time.Now().UTC().Format(time.RFC3339),
		"agent_url":      s.cfg.PublicAgentURL,
		"ca_fingerprint": s.ca.Fingerprint(),
		"nodes": map[string]int{
			"total":    len(nodes),
			"online":   s.nodes.OnlineCount(),
			"approved": approved,
		},
	})
}

func (s *Server) handleModules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, modules.Registry)
}

// handleEvents streams the event log as SSE. The Last-Event-ID header (or a
// ?after= query) resumes within the 500-event window; ?types= filters.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	var filter map[string]bool
	if types := r.URL.Query().Get("types"); types != "" {
		filter = map[string]bool{}
		for _, t := range strings.Split(types, ",") {
			if t = strings.TrimSpace(t); t != "" {
				filter[t] = true
			}
		}
	}
	var after int64
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			after = n
		}
	}
	if q := r.URL.Query().Get("after"); q != "" {
		if n, err := strconv.ParseInt(q, 10, 64); err == nil {
			after = n
		}
	}
	ch, cancel := s.bus.Subscribe(r.Context(), after)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if filter != nil && !filter[ev.Type] {
				continue
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, ev.Payload)
			flusher.Flush()
		}
	}
}
