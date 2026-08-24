package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/jobs"
)

func stringMap(value any) (map[string]string, bool) {
	if typed, ok := value.(map[string]string); ok {
		return typed, true
	}
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	result := make(map[string]string, len(raw))
	for key, value := range raw {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		result[key] = text
	}
	return result, true
}

func paperDigestSnapshot(root string) (map[string]string, error) {
	digests := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !editablePaperExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digests[filepath.ToSlash(relative)] = digestBytes(contents)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return digests, nil
	}
	return digests, err
}

func changedDigestPaths(before, after map[string]string) []string {
	seen := map[string]struct{}{}
	for path := range before {
		seen[path] = struct{}{}
	}
	for path := range after {
		seen[path] = struct{}{}
	}
	changed := make([]string, 0, len(seen))
	for path := range seen {
		if before[path] != after[path] {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

func filterDigests(all map[string]string, paths []string) map[string]string {
	result := map[string]string{}
	for _, path := range paths {
		if digest, ok := all[path]; ok {
			result[path] = digest
		}
	}
	return result
}

func gitChangedPaths(artifacts, baseline string) ([]string, error) {
	tracked, err := runGit(artifacts, "diff", "--name-only", baseline, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := runGit(artifacts, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, output := range [][]byte{tracked, untracked} {
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			line = filepath.ToSlash(strings.TrimSpace(line))
			if line != "" {
				seen[line] = struct{}{}
			}
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func restorePaperArtifacts(artifacts, baseline string) {
	_, _ = runGit(artifacts, "reset", "--hard", baseline)
	_, _ = runGit(artifacts, "clean", "-fd", "--", "workspace/project/paper")
	_, _ = runGit(artifacts, "push", "--force", "origin", baseline+":main")
}

func (m *Module) validatePaperBaseETags(projectID string, expected map[string]string) error {
	root := m.paperRoot(projectID)
	for requested, digest := range expected {
		path, err := securePaperPath(root, requested, true)
		if err != nil || !editablePaperExtensions[strings.ToLower(filepath.Ext(path))] {
			return fmt.Errorf("paper.edit_conflict: invalid base path %s", requested)
		}
		contents, err := os.ReadFile(path)
		if err != nil || normalizeETag(digest) != digestBytes(contents) {
			return fmt.Errorf("paper.edit_conflict: %s changed", requested)
		}
	}
	return nil
}

func (m *Module) runPaperEdit(ctx context.Context, job *jobs.Context) (map[string]any, error) {
	projectID, _ := job.Input["project_id"].(string)
	baseETags, ok := stringMap(job.Input["base_etags"])
	if !ok {
		return nil, errors.New("paper.edit_conflict: base_etags invalid")
	}
	if err := m.validatePaperBaseETags(projectID, baseETags); err != nil {
		return nil, err
	}
	artifacts := filepath.Join(m.projectRoot(projectID), "artifacts")
	if err := requireCleanArtifacts(artifacts); err != nil {
		return nil, err
	}
	baselineRaw, err := runGit(artifacts, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	baseline := strings.TrimSpace(string(baselineRaw))
	before, err := paperDigestSnapshot(m.paperRoot(projectID))
	if err != nil {
		return nil, err
	}

	output, err := m.executeWorker(ctx, job, "paper-edit")
	if err != nil {
		restorePaperArtifacts(artifacts, baseline)
		return nil, err
	}
	changedGit, err := gitChangedPaths(artifacts, baseline)
	if err != nil {
		restorePaperArtifacts(artifacts, baseline)
		return nil, err
	}
	for _, path := range changedGit {
		if path != "workspace/project/paper" && !strings.HasPrefix(path, "workspace/project/paper/") {
			restorePaperArtifacts(artifacts, baseline)
			return nil, fmt.Errorf("autoresearch.paper_edit_scope: %s", path)
		}
	}
	after, err := paperDigestSnapshot(m.paperRoot(projectID))
	if err != nil {
		restorePaperArtifacts(artifacts, baseline)
		return nil, err
	}
	changed := changedDigestPaths(before, after)
	if status, statusErr := runGit(artifacts, "status", "--porcelain"); statusErr != nil {
		restorePaperArtifacts(artifacts, baseline)
		return nil, statusErr
	} else if strings.TrimSpace(string(status)) != "" {
		commitPaths := make([]string, 0, len(changed))
		for _, path := range changed {
			commitPaths = append(commitPaths, filepath.ToSlash(filepath.Join("workspace", "project", "paper", filepath.FromSlash(path))))
		}
		if len(commitPaths) == 0 {
			restorePaperArtifacts(artifacts, baseline)
			return nil, errors.New("autoresearch.paper_edit_scope: non-source changes")
		}
		if err := commitArtifacts(artifacts, "paper: apply writer chat edit", commitPaths...); err != nil {
			restorePaperArtifacts(artifacts, baseline)
			return nil, err
		}
	} else if len(changed) > 0 {
		if _, err := runGit(artifacts, "push", "origin", "main"); err != nil {
			return nil, err
		}
	}

	writerBody := "Paper writer completed the requested edit."
	if contents, err := os.ReadFile(filepath.Join(m.projectRoot(projectID), "scratch", job.RunID, "dispatcher-output.txt")); err == nil && strings.TrimSpace(string(contents)) != "" {
		writerBody = strings.TrimSpace(string(contents))
	}
	changedJSON, _ := json.Marshal(changed)
	messageID, err := id.New()
	if err != nil {
		return nil, err
	}
	if err := m.env.Q.CreateAutoResearchMessage(ctx, db.CreateAutoResearchMessageParams{
		ID: messageID, ProjectID: projectID, Role: "writer", Body: writerBody, ChangedPathsJson: string(changedJSON),
	}); err != nil {
		return nil, err
	}
	output["changed_paths"] = changed
	output["before_digests"] = filterDigests(before, changed)
	output["after_digests"] = filterDigests(after, changed)
	return output, nil
}

func (m *Module) runPaperCompile(ctx context.Context, job *jobs.Context) (map[string]any, error) {
	projectID, _ := job.Input["project_id"].(string)
	artifacts := filepath.Join(m.projectRoot(projectID), "artifacts")
	if err := requireCleanArtifacts(artifacts); err != nil {
		return nil, err
	}
	baselineRaw, err := runGit(artifacts, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	baseline := strings.TrimSpace(string(baselineRaw))
	output, err := m.executeWorker(ctx, job, "paper-compile")
	if err != nil {
		restorePaperArtifacts(artifacts, baseline)
		return nil, err
	}
	changed, err := gitChangedPaths(artifacts, baseline)
	if err != nil {
		restorePaperArtifacts(artifacts, baseline)
		return nil, err
	}
	for _, path := range changed {
		if path != "workspace/project/paper" && !strings.HasPrefix(path, "workspace/project/paper/") {
			restorePaperArtifacts(artifacts, baseline)
			return nil, fmt.Errorf("autoresearch.paper_compile_scope: %s", path)
		}
	}
	if status, statusErr := runGit(artifacts, "status", "--porcelain"); statusErr != nil {
		restorePaperArtifacts(artifacts, baseline)
		return nil, statusErr
	} else if strings.TrimSpace(string(status)) != "" {
		if len(changed) == 0 {
			restorePaperArtifacts(artifacts, baseline)
			return nil, errors.New("autoresearch.paper_compile_scope: changes unavailable")
		}
		if err := commitArtifacts(artifacts, "paper: compile manuscript", changed...); err != nil {
			restorePaperArtifacts(artifacts, baseline)
			return nil, err
		}
	} else if len(changed) > 0 {
		if _, err := runGit(artifacts, "push", "origin", "main"); err != nil {
			return nil, err
		}
	}
	paper := filepath.Join(m.paperRoot(projectID), "build", "manuscript.pdf")
	contents, err := os.ReadFile(paper)
	if err != nil {
		return nil, errors.New("autoresearch.paper_compile_missing")
	}
	if len(contents) < 5 || !strings.HasPrefix(string(contents[:5]), "%PDF-") {
		return nil, errors.New("autoresearch.paper_pdf_invalid")
	}
	output["changed_paths"] = []string{"build/manuscript.pdf", "build/compile.log"}
	output["paper_path"] = "workspace/project/paper/build/manuscript.pdf"
	return output, nil
}
