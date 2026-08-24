package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jj-link/local-model-works/internal/db"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func runGit(dir string, args ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func initializeProjectRoot(root string) error {
	if root == "" {
		return errors.New("autoresearch root is not configured")
	}
	artifacts := filepath.Join(root, "artifacts")
	origin := filepath.Join(root, "origin.git")
	for _, directory := range []string{
		filepath.Join(root, ".lmw", "sources"), filepath.Join(root, "scratch"),
		filepath.Join(artifacts, "ideas"), filepath.Join(artifacts, "topics"), filepath.Join(artifacts, "workspace"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	for path, contents := range map[string][]byte{
		filepath.Join(root, ".lmw", "sessions.json"): []byte("{}\n"),
		filepath.Join(root, ".lmw", "config.json"):   []byte("{}\n"),
	} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				return err
			}
		}
	}
	if _, err := os.Stat(filepath.Join(origin, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(origin, 0o700); err != nil {
			return err
		}
		command := exec.Command("git", "init", "--bare", "--initial-branch=main", origin)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("initialize bare origin: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	if _, err := os.Stat(filepath.Join(artifacts, ".git")); errors.Is(err, os.ErrNotExist) {
		command := exec.Command("git", "init", "--initial-branch=main", artifacts)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("initialize artifacts: %w: %s", err, strings.TrimSpace(string(output)))
		}
		for _, args := range [][]string{{"config", "user.name", "Local Model Works"}, {"config", "user.email", "autoresearch@local.invalid"}, {"remote", "add", "origin", origin}} {
			if _, err := runGit(artifacts, args...); err != nil {
				return err
			}
		}
		manifest, _ := json.MarshalIndent(map[string]any{"schema": 1, "managed_by": "local-model-works"}, "", "  ")
		if err := os.WriteFile(filepath.Join(artifacts, ".lmw-project.json"), append(manifest, '\n'), 0o600); err != nil {
			return err
		}
		for _, directory := range []string{"ideas", "topics", "workspace"} {
			if err := os.WriteFile(filepath.Join(artifacts, directory, ".gitkeep"), nil, 0o600); err != nil {
				return err
			}
		}
		if _, err := runGit(artifacts, "add", ".lmw-project.json", "ideas/.gitkeep", "topics/.gitkeep", "workspace/.gitkeep"); err != nil {
			return err
		}
		if _, err := runGit(artifacts, "commit", "-m", "autoresearch: initialize project artifacts"); err != nil {
			return err
		}
		if _, err := runGit(artifacts, "push", "-u", "origin", "main"); err != nil {
			return err
		}
	}
	return nil
}

func artifactSlug(title, id string) string {
	slug := nonSlug.ReplaceAllString(strings.ToLower(title), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 48 {
		slug = strings.TrimRight(slug[:48], "-")
	}
	if slug == "" {
		slug = "idea"
	}
	short := strings.ReplaceAll(id, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return slug + "-" + short
}

func requireCleanArtifacts(artifacts string) error {
	output, err := runGit(artifacts, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != "" {
		return errors.New("autoresearch.artifacts_dirty")
	}
	return nil
}

func commitArtifacts(artifacts, message string, paths ...string) error {
	if _, err := runGit(artifacts, append([]string{"add", "--"}, paths...)...); err != nil {
		return err
	}
	if _, err := runGit(artifacts, "commit", "-m", message); err != nil {
		return err
	}
	_, err := runGit(artifacts, "push", "origin", "main")
	return err
}

func adoptIdea(root string, idea db.AutoresearchIdea) error {
	artifacts := filepath.Join(root, "artifacts")
	if err := requireCleanArtifacts(artifacts); err != nil {
		return err
	}
	slug := artifactSlug(idea.Title, idea.ID)
	ideaPath := filepath.Join(artifacts, "ideas", slug+".v1.md")
	contents := fmt.Sprintf("# %s\n\n%s\n", idea.Title, strings.TrimSpace(idea.Body))
	if err := os.WriteFile(ideaPath, []byte(contents), 0o600); err != nil {
		return err
	}
	indexPath := filepath.Join(artifacts, "ideas", "ideas.xml")
	index := "<ideas>\n"
	if existing, err := os.ReadFile(indexPath); err == nil {
		index = strings.TrimSuffix(string(existing), "</ideas>\n")
	}
	index += fmt.Sprintf("  <idea slug=%q topic=%q current_version=\"1\" score=\"none\" />\n</ideas>\n", slug, "project")
	if err := os.WriteFile(indexPath, []byte(index), 0o600); err != nil {
		return err
	}
	return commitArtifacts(artifacts, "idea: adopt "+slug, filepath.ToSlash(filepath.Join("ideas", slug+".v1.md")), "ideas/ideas.xml")
}
