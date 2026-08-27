package recipe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
)

// RepositorySource is the exact immutable source presented to a compiler.
type RepositorySource struct {
	RepositoryID string
	URL          string
	Path         string
	CommitSHA    string
	TreeSHA      string
}

// RepositoryCompiler deterministically maps one checked-out commit to an
// immutable recipe package without executing repository code.
type RepositoryCompiler interface {
	Compile(ctx context.Context, source RepositorySource, checkout string, previous *RecipeDetail) (*PackResult, error)
}

// RepositoryCompilerRegistry selects a compiler by normalized repository
// identity or by the presence of a native recipe bundle.
type RepositoryCompilerRegistry interface {
	Lookup(source RepositorySource, checkout string) (RepositoryCompiler, bool)
}

// SetRepositoryCompilerRegistry installs the controller's deterministic
// compiler registry. It is configured once during server construction.
func (s *Service) SetRepositoryCompilerRegistry(registry RepositoryCompilerRegistry) {
	s.repositoryCompilers = registry
}

// InstallRepositoryCommit compiles and installs one expected upstream commit.
// It refuses a moved tracked ref before compiling or touching deployments.
func (s *Service) InstallRepositoryCommit(ctx context.Context, repositoryID, expectedCommit string) (*Recipe, error) {
	repository, err := s.q.GetRecipeRepository(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	expectedCommit = strings.ToLower(strings.TrimSpace(expectedCommit))
	if !sha40.MatchString(expectedCommit) {
		return nil, fmt.Errorf("%w: %q is not a 40-hex commit", ErrUnpinnedRevision, expectedCommit)
	}
	resolved, err := resolveGitRemoteRef(ctx, repository.SourceUrl, repository.TrackingRef)
	if err != nil {
		return nil, err
	}
	if resolved != expectedCommit {
		return nil, &PackError{Code: "recipe.update_stale", Message: fmt.Sprintf("tracked ref moved to %s", resolved)}
	}

	checkout, err := os.MkdirTemp("", "lmw-repository-update-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(checkout)
	cloneArgs := []string{"clone", "--no-checkout"}
	if isLocalGitPath(repository.SourceUrl) {
		cloneArgs = append(cloneArgs, "--no-hardlinks")
	}
	cloneArgs = append(cloneArgs, repository.SourceUrl, checkout)
	if err := runGit(ctx, checkout, cloneArgs...); err != nil {
		return nil, fmt.Errorf("git clone: %w", err)
	}
	if err := runGit(ctx, checkout, "checkout", "--quiet", expectedCommit); err != nil {
		return nil, fmt.Errorf("git checkout %s: %w", expectedCommit, err)
	}
	commitSHA, err := runGitOutput(ctx, checkout, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if commitSHA != expectedCommit {
		return nil, fmt.Errorf("git checkout resolved %s, expected %s", commitSHA, expectedCommit)
	}
	treeSHA, err := runGitOutput(ctx, checkout, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}

	source := RepositorySource{
		RepositoryID: repository.ID,
		URL:          repository.SourceUrl,
		Path:         repository.SourcePath,
		CommitSHA:    commitSHA,
		TreeSHA:      treeSHA,
	}
	if s.repositoryCompilers == nil {
		return nil, &PackError{Code: RepositoryUnsupportedCode, Message: "repository has no deterministic compiler"}
	}
	compiler, ok := s.repositoryCompilers.Lookup(source, checkout)
	if !ok {
		return nil, &PackError{Code: RepositoryUnsupportedCode, Message: "repository has no native recipe bundle or registered deterministic compiler"}
	}
	var previous *RecipeDetail
	if repository.CurrentDigest.Valid {
		detail, getErr := s.Get(ctx, repository.CurrentDigest.String)
		if getErr != nil {
			return nil, getErr
		}
		previous = &detail
	}
	packed, err := compiler.Compile(ctx, source, checkout, previous)
	if err != nil {
		return nil, err
	}
	manifest, diagnostics, err := s.validator.ValidateStrict(packed.ConfigJSON)
	if err != nil {
		return nil, err
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			return nil, fmt.Errorf("recipe validation: %s", diagnostic.Message)
		}
	}
	if manifest.Metadata.Source == nil {
		return nil, fmt.Errorf("compiled recipe is missing metadata.source")
	}
	compiledID, _, _, err := RepositoryIdentity(*manifest.Metadata.Source)
	if err != nil {
		return nil, err
	}
	if compiledID != repository.ID || !strings.EqualFold(manifest.Metadata.Source.Revision, commitSHA) {
		return nil, fmt.Errorf("compiled recipe source does not match repository commit")
	}
	installed, err := s.storePack(ctx, packed, RecipeSource{
		Type: "git", Remote: repository.SourceUrl, Path: repository.SourcePath,
		Revision: commitSHA, Tree: treeSHA,
	}, TrustUntrusted)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.q.SetRecipeRepositoryHead(ctx, db.SetRecipeRepositoryHeadParams{
		TrackingRef: repository.TrackingRef, ObservedHeadCommit: nullableString(commitSHA),
		ObservedHeadTree: nullableString(treeSHA), HeadCheckedAt: nullableString(now),
		UpdatedAt: now, ID: repository.ID,
	}); err != nil {
		return nil, err
	}
	return &installed, nil
}

func resolveGitRemoteRef(ctx context.Context, remote, trackingRef string) (string, error) {
	ref := "HEAD"
	if trackingRef != "" && trackingRef != "HEAD" {
		ref = "refs/heads/" + strings.TrimPrefix(trackingRef, "refs/heads/")
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", remote, ref)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve repository ref: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || !sha40.MatchString(fields[0]) {
		return "", fmt.Errorf("resolve repository ref: no commit for %s", ref)
	}
	return strings.ToLower(fields[0]), nil
}

func repositoryCheckoutPath(checkout, sourcePath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(sourcePath))
	if clean == "." {
		return checkout, nil
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repository source path %q escapes checkout", sourcePath)
	}
	return filepath.Join(checkout, clean), nil
}
