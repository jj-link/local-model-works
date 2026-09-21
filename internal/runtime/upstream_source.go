package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const upstreamInventoryLimit = 4 << 20

// Include authored source additions, but not dependency/model/build caches. Git
// ignored files are excluded unless explicitly named as configuration inputs.
func upstreamCachePath(relative string) bool {
	for _, part := range strings.Split(relative, "/") {
		switch strings.ToLower(part) {
		case ".git", ".cache", "cache", "caches", "node_modules", ".venv", "venv", "__pycache__", "models", "weights", "checkpoints", "downloads", "build", "dist":
			return true
		}
	}
	switch strings.ToLower(filepath.Ext(relative)) {
	case ".gguf", ".safetensors", ".onnx", ".pt", ".pth", ".pyc":
		return true
	}
	return false
}
func upstreamUntrackedSource(relative string) bool {
	if upstreamCachePath(relative) {
		return false
	}
	name := filepath.Base(relative)
	if strings.HasPrefix(name, ".env") {
		return true
	}
	switch name {
	case "Dockerfile", "Containerfile", "Makefile", "CMakeLists.txt", "requirements.txt", ".gitignore", ".dockerignore":
		return true
	}
	switch strings.ToLower(filepath.Ext(relative)) {
	case ".sh", ".bash", ".py", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".go", ".rs", ".c", ".h", ".cc", ".cpp", ".hpp", ".cu", ".cuh", ".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".env", ".sql", ".patch", ".diff":
		return true
	}
	return false
}

func upstreamGitOutput(ctx context.Context, repository string, limit int64, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "--literal-pathspecs", "-C", repository}, args...)...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, limit+1))
	if readErr != nil || int64(len(data)) > limit {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if readErr != nil {
			return nil, fmt.Errorf("upstream.source_read: %w", readErr)
		}
		return nil, fmt.Errorf("upstream.source_read_limit")
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("upstream.source_read: %w", err)
	}
	return data, nil
}

func upstreamSourceFiles(ctx context.Context, spec *ContainerSpec, repository string, retained ...string) ([]string, error) {
	paths := make(map[string]bool)
	for _, args := range [][]string{
		{"ls-files", "--cached", "--recurse-submodules", "-z"},
		{"ls-tree", "-r", "--name-only", "-z", "HEAD"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		data, err := upstreamGitOutput(ctx, repository, upstreamInventoryLimit, args...)
		if err != nil {
			return nil, err
		}
		for _, relative := range strings.Split(string(data), "\x00") {
			if relative == "" || (args[1] == "--others" && !upstreamUntrackedSource(relative)) {
				continue
			}
			paths[relative] = true
		}
	}
	// Carried tracked additions need not remain staged in the successor index.
	// Their recorded paths retain integrity coverage even for opaque extensions.
	var recorded map[string]string
	if err := readUpstreamJSON(filepath.Join(filepath.Dir(repository), "source-state.json"), &recorded); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for relative := range recorded {
		paths[relative] = true
	}
	for _, relative := range retained {
		paths[relative] = true
	}
	if spec.Upstream.EnvFile != "" {
		paths[filepath.ToSlash(filepath.Join(spec.Upstream.SourcePath, spec.Upstream.EnvFile))] = true
	}
	for _, file := range spec.Upstream.Configuration {
		paths[filepath.ToSlash(filepath.Join(spec.Upstream.SourcePath, file.Path))] = true
	}
	if spec.Upstream.LogFile != "" {
		delete(paths, filepath.ToSlash(filepath.Join(spec.Upstream.SourcePath, spec.Upstream.LogFile)))
	}
	result := make([]string, 0, len(paths))
	for relative := range paths {
		result = append(result, relative)
	}
	sort.Strings(result)
	return result, nil
}

// Never traverse a symlink, including an intermediate directory. The final link
// itself can be inventoried; preservation separately validates its destination.
func upstreamSourcePath(repository, relative string) (string, error) {
	if !safeUpstreamRelative(relative) || relative == "." || filepath.ToSlash(filepath.Clean(relative)) != relative {
		return "", fmt.Errorf("upstream.source_inventory_path_invalid: %s", relative)
	}
	current := repository
	parts := strings.Split(relative, "/")
	for i, part := range parts {
		if strings.EqualFold(part, ".git") {
			return "", fmt.Errorf("upstream.source_inventory_path_invalid: %s", relative)
		}
		current = filepath.Join(current, part)
		if i == len(parts)-1 {
			break
		}
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("upstream.source_parent_not_directory: %s", relative)
		}
	}
	return current, nil
}

// Fingerprints authenticate working bytes after supervised authored commands.
// Immutable Git blobs remain the merge base; source-state is not a new origin.
func upstreamSourceFingerprint(ctx context.Context, spec *ContainerSpec, repository string, retained ...string) (map[string]string, error) {
	for _, path := range []string{repository, filepath.Join(repository, ".git")} {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("upstream.source_symlink")
		}
	}
	files, err := upstreamSourceFiles(ctx, spec, repository, retained...)
	if err != nil {
		return nil, err
	}
	state := make(map[string]string)
	for _, relative := range files {
		path, err := upstreamSourcePath(repository, relative)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			state[relative] = "missing"
			continue
		}
		if err != nil {
			return nil, err
		}
		// ls-tree also lists gitlinks. Their files and pinned commit are checked
		// separately, and a directory is never copied as a customization.
		if info.IsDir() {
			entry, err := upstreamGitOutput(ctx, repository, upstreamInventoryLimit, "ls-tree", "-z", "HEAD", "--", relative)
			if err != nil || !strings.HasPrefix(string(entry), "160000 commit ") {
				return nil, fmt.Errorf("upstream.source_file_not_regular: %s", relative)
			}
			continue
		}
		hash := sha256.New()
		_, _ = fmt.Fprintf(hash, "%s\x00", info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return nil, err
			}
			_, _ = io.WriteString(hash, target)
		} else {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("upstream.source_file_not_regular: %s", relative)
			}
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(hash, file)
			_ = file.Close()
			if err != nil {
				return nil, err
			}
		}
		state[relative] = hex.EncodeToString(hash.Sum(nil))
	}
	return state, nil
}

func recordUpstreamSource(ctx context.Context, spec *ContainerSpec, repository string, retained ...string) error {
	state, err := upstreamSourceFingerprint(ctx, spec, repository, retained...)
	if err != nil {
		return err
	}
	return writeUpstreamJSON(filepath.Join(filepath.Dir(repository), "source-state.json"), state)
}
