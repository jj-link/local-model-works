package recipe

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
)

const (
	DefaultUpdateCheckInterval = 6 * time.Hour
	PageUpdateCheckMaxAge      = 15 * time.Minute
	gitUpdateCheckTimeout      = 20 * time.Second
)

// UpdateStatus is the cached comparison between an installed immutable recipe
// revision and the tracked upstream repository head.
type UpdateStatus struct {
	State             string `json:"state"`
	Remote            string `json:"remote"`
	TrackingRef       string `json:"tracking_ref"`
	Path              string `json:"path,omitempty"`
	InstalledRevision string `json:"installed_revision"`
	CandidateRevision string `json:"candidate_revision,omitempty"`
	CheckedAt         string `json:"checked_at"`
	Error             string `json:"error,omitempty"`
}

type gitHead struct {
	ref      string
	revision string
	err      error
}

// UpdateStatus returns the last persisted upstream comparison for a recipe.
func (s *Service) UpdateStatus(ctx context.Context, digest string) (*UpdateStatus, error) {
	row, err := s.q.GetRecipeUpdateCheck(ctx, digest)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	status := updateStatusFromRow(row)
	return &status, nil
}

// CheckUpdatesNow synchronously refreshes every current GitHub-backed recipe.
func (s *Service) CheckUpdatesNow(ctx context.Context) ([]UpdateStatus, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	return s.checkUpdates(ctx, 0, true)
}

// RefreshUpdatesAsync starts one stale-only refresh. Concurrent page loads
// share the in-flight refresh instead of spawning duplicate Git requests.
func (s *Service) RefreshUpdatesAsync(ctx context.Context, maxAge time.Duration) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.updateMu.TryLock() {
		return false
	}
	go func() {
		defer s.updateMu.Unlock()
		_, _ = s.checkUpdates(ctx, maxAge, false)
	}()
	return true
}

// RunUpdateChecker refreshes stale status at startup and periodically until
// the controller context is cancelled.
func (s *Service) RunUpdateChecker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultUpdateCheckInterval
	}
	s.RefreshUpdatesAsync(ctx, interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RefreshUpdatesAsync(ctx, interval)
		}
	}
}

func (s *Service) checkUpdates(ctx context.Context, maxAge time.Duration, force bool) ([]UpdateStatus, error) {
	rows, err := s.q.ListRecipes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	seenNames := make(map[string]struct{}, len(rows))
	heads := make(map[string]gitHead)
	statuses := make([]UpdateStatus, 0, len(rows))

	for i := range rows {
		row := rows[i]
		if _, seen := seenNames[row.Name]; seen {
			continue
		}
		seenNames[row.Name] = struct{}{}

		var manifest Manifest
		if err := json.Unmarshal([]byte(row.Manifest), &manifest); err != nil || manifest.Metadata.Source == nil {
			continue
		}
		source := manifest.Metadata.Source
		remote, ok := normalizeGitHubRemote(source.URL)
		if !ok || !sha40.MatchString(source.Revision) {
			continue
		}

		if !force {
			cached, cacheErr := s.q.GetRecipeUpdateCheck(ctx, row.Digest)
			if cacheErr == nil && cached.Remote == remote && cached.InstalledRevision == source.Revision {
				checkedAt, parseErr := time.Parse(time.RFC3339Nano, cached.CheckedAt)
				if parseErr == nil && maxAge > 0 && now.Sub(checkedAt) < maxAge {
					statuses = append(statuses, updateStatusFromRow(cached))
					continue
				}
			} else if cacheErr != nil && cacheErr != sql.ErrNoRows {
				return nil, cacheErr
			}
		}

		head, resolved := heads[remote]
		if !resolved {
			resolveCtx, cancel := context.WithTimeout(ctx, gitUpdateCheckTimeout)
			head.ref, head.revision, head.err = s.resolveGitHead(resolveCtx, remote)
			cancel()
			heads[remote] = head
		}

		status := UpdateStatus{
			State:             "current",
			Remote:            remote,
			TrackingRef:       head.ref,
			Path:              source.Path,
			InstalledRevision: strings.ToLower(source.Revision),
			CheckedAt:         now.Format(time.RFC3339Nano),
		}
		if head.err != nil {
			status.State = "error"
			status.TrackingRef = "HEAD"
			status.Error = head.err.Error()
		} else if !strings.EqualFold(head.revision, source.Revision) {
			status.State = "available"
			status.CandidateRevision = head.revision
		}
		if err := s.persistUpdateStatus(ctx, row.Digest, status); err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
		s.bus.Publish(ctx, "recipe.update_checked", row.Digest, mustJSON(status))
	}
	return statuses, nil
}

func (s *Service) persistUpdateStatus(ctx context.Context, digest string, status UpdateStatus) error {
	return s.q.UpsertRecipeUpdateCheck(ctx, db.UpsertRecipeUpdateCheckParams{
		RecipeDigest:      digest,
		Remote:            status.Remote,
		TrackingRef:       status.TrackingRef,
		Path:              status.Path,
		InstalledRevision: status.InstalledRevision,
		CandidateRevision: nullableString(status.CandidateRevision),
		State:             status.State,
		CheckedAt:         status.CheckedAt,
		Error:             nullableString(status.Error),
	})
}

func updateStatusFromRow(row db.RecipeUpdateCheck) UpdateStatus {
	return UpdateStatus{
		State:             row.State,
		Remote:            row.Remote,
		TrackingRef:       row.TrackingRef,
		Path:              row.Path,
		InstalledRevision: row.InstalledRevision,
		CandidateRevision: nullStrValue(row.CandidateRevision),
		CheckedAt:         row.CheckedAt,
		Error:             nullStrValue(row.Error),
	}
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func normalizeGitHubRemote(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "github.com") || u.User != nil || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return "https://github.com/" + parts[0] + "/" + parts[1] + ".git", true
}

func resolveGitHEAD(ctx context.Context, remote string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--symref", remote, "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", "", fmt.Errorf("GitHub update check: %w", ctx.Err())
		}
		return "", "", fmt.Errorf("GitHub update check: %w", err)
	}
	ref := "HEAD"
	revision := ""
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
			ref = strings.TrimPrefix(fields[1], "refs/heads/")
		}
		if len(fields) == 2 && fields[1] == "HEAD" && sha40.MatchString(fields[0]) {
			revision = strings.ToLower(fields[0])
		}
	}
	if revision == "" {
		return "", "", fmt.Errorf("GitHub update check returned no HEAD revision")
	}
	return ref, revision, nil
}
