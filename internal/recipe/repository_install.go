package recipe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// ActivateRepositoryVersion selects one installed version after all external
// update work has completed successfully.
func ActivateRepositoryVersion(ctx context.Context, q *db.Queries, repositoryID, digest string) error {
	version, err := q.GetRecipeRepositoryVersionByDigest(ctx, digest)
	if err != nil {
		return err
	}
	if version.RepositoryID != repositoryID {
		return fmt.Errorf("recipe repository: digest %s belongs to %s", digest, version.RepositoryID)
	}
	return q.SetRecipeRepositoryCurrent(ctx, db.SetRecipeRepositoryCurrentParams{
		CurrentDigest: nullableString(digest),
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		ID:            repositoryID,
	})
}

func resolveGitRemoteRef(ctx context.Context, remote, trackingRef string) (string, error) {
	ref := strings.TrimSpace(trackingRef)
	if ref == "" {
		ref = "HEAD"
	}
	refs := []string{ref}
	if ref != "HEAD" && !strings.HasPrefix(ref, "refs/") {
		refs = []string{"refs/heads/" + ref, "refs/tags/" + ref, "refs/tags/" + ref + "^{}"}
	} else if strings.HasPrefix(ref, "refs/tags/") {
		refs = append(refs, ref+"^{}")
	}
	args := append([]string{"ls-remote", "--", remote}, refs...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve repository ref: %w", err)
	}
	commits := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && sha40.MatchString(fields[0]) {
			commits[fields[1]] = strings.ToLower(fields[0])
		}
	}
	if len(refs) == 3 && commits[refs[0]] != "" && commits[refs[1]] != "" {
		return "", fmt.Errorf("resolve repository ref: %s is both a branch and tag; select its full ref", ref)
	}
	for index := len(refs) - 1; index >= 0; index-- {
		if commit := commits[refs[index]]; commit != "" {
			return commit, nil
		}
	}
	return "", fmt.Errorf("resolve repository ref: no commit for %s", ref)
}
