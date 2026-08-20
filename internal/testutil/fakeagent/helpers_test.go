package fakeagent

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/deploy"
)

// ---------------------------------------------------------------------------
// Deployment polling
// ---------------------------------------------------------------------------

// depState returns a deployment's observed state ("" when missing).
func depState(s *Server, depID string) string {
	d, err := s.Srv.Deployments().Get(s.Ctx, depID)
	if err != nil {
		return ""
	}
	return d.ObservedState
}

// depGet fetches one deployment view.
func depGet(t *testing.T, s *Server, depID string) deploy.Deployment {
	t.Helper()
	d, err := s.Srv.Deployments().Get(s.Ctx, depID)
	if err != nil {
		t.Fatalf("get deployment %s: %v", depID, err)
	}
	return *d
}

// waitDep blocks until the deployment's observed state equals want.
func waitDep(t *testing.T, s *Server, depID, want string) deploy.Deployment {
	t.Helper()
	Deadline(t, 30*time.Second, func() bool {
		return depState(s, depID) == want
	}, fmt.Sprintf("deployment %s observed state %q", depID, want))
	return depGet(t, s, depID)
}

// install is the digest-only InstallRecipe shortcut.
func install(t *testing.T, s *Server, r FixtureRecipe) string {
	t.Helper()
	d, _ := InstallRecipe(t, s, r)
	return d
}

// createDep creates a deployment from an installed digest, optionally with
// operator-pinned rank->node placements.
func createDep(t *testing.T, s *Server, digest string, ov ...deploy.PlacementOverride) deploy.Deployment {
	t.Helper()
	d, err := s.Srv.Deployments().Create(s.Ctx, deploy.CreateRequest{RecipeDigest: digest, Placements: ov})
	if err != nil {
		t.Fatalf("create deployment (%s): %v", digest, err)
	}
	return *d
}

// shortID mirrors the production container-name derivation (internal/deploy
// service.go shortID): strip the kind prefix, take eight chars.
func shortID(id string) string {
	id = strings.TrimPrefix(id, "deployment:")
	id = strings.TrimPrefix(id, "run:")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// containersOf lists a deployment's stub containers by their deterministic
// names (lmw-<dep8>-<run8>-r<rank>), keyed by rank.
func containersOf(rt *FakeRuntime, dep deploy.Deployment) map[int32]*FakeContainer {
	prefix := "lmw-" + shortID(dep.ID) + "-" + shortID(dep.RunID) + "-"
	out := map[int32]*FakeContainer{}
	for _, name := range rt.Containers() {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := name[len(prefix):] // "r<rank>"
		if len(rest) < 2 || rest[0] != 'r' {
			continue
		}
		var rank int32
		if _, err := fmt.Sscanf(rest[1:], "%d", &rank); err != nil {
			continue
		}
		out[rank] = rt.Container(name)
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP session + SSE
// ---------------------------------------------------------------------------

// login returns a client carrying an authenticated admin session cookie.
func login(t *testing.T, s *Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	c := &http.Client{Jar: jar}
	resp, err := c.Post("http://"+s.HTTPAddr+"/api/v1/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"test-password"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: status %d", resp.StatusCode)
	}
	return c
}

type sseEvent struct {
	id   string
	kind string // "" for message events, "end" for the terminal event
	data string
}

// readSSE consumes GET /api/v1/deployments/{id}/logs?rank=N with a login
// session and Last-Event-ID resume, invoking onEvent per event. Returning
// true from onEvent stops the read early. It returns the events received
// and whether the server's terminal "end" event arrived.
func readSSE(t *testing.T, client *http.Client, s *Server, depID string, rank int32, lastEventID string, timeout time.Duration, onEvent func(sseEvent) bool) (events []sseEvent, ended bool) {
	t.Helper()
	url := fmt.Sprintf("http://%s/api/v1/deployments/%s/logs?rank=%d", s.HTTPAddr, depID, rank)
	ctx, cancel := context.WithTimeout(s.Ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("sse request: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse: status %d", resp.StatusCode)
	}
	rd := bufio.NewReader(resp.Body)
	var ev sseEvent
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("sse read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if ev.id != "" || ev.data != "" || ev.kind != "" {
				events = append(events, ev)
				if ev.kind == "end" {
					return events, true
				}
				if onEvent != nil && onEvent(ev) {
					return events, false
				}
			}
			ev = sseEvent{}
			continue
		}
		if strings.HasPrefix(line, ":") { // SSE comment (keepalive)
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			ev.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			ev.kind = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
}
