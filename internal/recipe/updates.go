package recipe

import (
	"context"
	"database/sql"
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

// UpdateStatus is the cached comparison between the installed repository
// version and the last observed upstream HEAD.
type UpdateStatus struct {
	State             string `json:"state"`
	RepositoryID      string `json:"repository_id"`
	Remote            string `json:"remote"`
	TrackingRef       string `json:"tracking_ref"`
	Path              string `json:"path,omitempty"`
	InstalledRevision string `json:"installed_revision"`
	CandidateRevision string `json:"candidate_revision,omitempty"`
	CheckedAt         string `json:"checked_at,omitempty"`
	Error             string `json:"error,omitempty"`
}

type gitHead struct {
	ref      string
	revision string
	err      error
}

// UpdateStatus returns the repository-level upstream comparison for a recipe
// digest. Unlinked local packages have no update status.
func (s *Service) UpdateStatus(ctx context.Context, digest string) (*UpdateStatus, error) {
	version, err := s.q.GetRecipeRepositoryVersionByDigest(ctx, digest)
	if err != nil {
		if errorsIsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	repository, err := s.q.GetRecipeRepository(ctx, version.RepositoryID)
	if err != nil {
		return nil, err
	}
	if !repository.HeadCheckedAt.Valid {
		return nil, nil
	}
	status := updateStatusFromRepository(repository, version.CommitSha)
	return &status, nil
}

// CheckUpdatesNow synchronously refreshes every repository-backed recipe.
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
	repositories, err := s.q.ListRecipeRepositories(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	heads := make(map[string]gitHead, len(repositories))
	statuses := make([]UpdateStatus, 0, len(repositories))

	for _, repository := range repositories {
		if !repository.CurrentDigest.Valid {
			continue
		}
		versions, err := s.q.ListRecipeRepositoryVersions(ctx, repository.ID)
		if err != nil {
			return nil, err
		}
		installedCommit := ""
		for _, version := range versions {
			if version.RecipeDigest == repository.CurrentDigest.String {
				installedCommit = version.CommitSha
				break
			}
		}
		if installedCommit == "" {
			continue
		}
		remote, ok := normalizeGitHubRemote(repository.SourceUrl)
		if !ok || !sha40.MatchString(installedCommit) {
			continue
		}

		if !force && repository.HeadCheckedAt.Valid {
			checkedAt, parseErr := time.Parse(time.RFC3339Nano, repository.HeadCheckedAt.String)
			if parseErr == nil && maxAge > 0 && now.Sub(checkedAt) < maxAge {
				statuses = append(statuses, updateStatusFromRepository(repository, installedCommit))
				continue
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
			RepositoryID:      repository.ID,
			Remote:            repository.SourceUrl,
			TrackingRef:       head.ref,
			Path:              repository.SourcePath,
			InstalledRevision: installedCommit,
			CheckedAt:         now.Format(time.RFC3339Nano),
		}
		if head.err != nil {
			status.State = "error"
			status.TrackingRef = repository.TrackingRef
			status.Error = head.err.Error()
		} else {
			status.CandidateRevision = head.revision
			if !strings.EqualFold(head.revision, installedCommit) {
				status.State = "available"
			}
			if err := s.q.SetRecipeRepositoryHead(ctx, db.SetRecipeRepositoryHeadParams{
				TrackingRef:        head.ref,
				ObservedHeadCommit: nullableString(head.revision),
				ObservedHeadTree:   sql.NullString{},
				HeadCheckedAt:      nullableString(status.CheckedAt),
				UpdatedAt:          status.CheckedAt,
				ID:                 repository.ID,
			}); err != nil {
				return nil, err
			}
		}
		statuses = append(statuses, status)
		s.bus.Publish(ctx, "recipe.update_checked", repository.ID, mustJSON(status))
	}
	return statuses, nil
}

func updateStatusFromRepository(row db.RecipeRepository, installedCommit string) UpdateStatus {
	status := UpdateStatus{
		State:             "current",
		RepositoryID:      row.ID,
		Remote:            row.SourceUrl,
		TrackingRef:       row.TrackingRef,
		Path:              row.SourcePath,
		InstalledRevision: installedCommit,
		CandidateRevision: nullStrValue(row.ObservedHeadCommit),
		CheckedAt:         nullStrValue(row.HeadCheckedAt),
	}
	if row.ObservedHeadCommit.Valid && !strings.EqualFold(row.ObservedHeadCommit.String, installedCommit) {
		status.State = "available"
	}
	return status
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func errorsIsNoRows(err error) bool {
	return err == sql.ErrNoRows
}

func normalizeGitHubRemote(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "github.com") || u.User != nil || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return "https://github.com/" + parts[0] + "/" + parts[1], true
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
