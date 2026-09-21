package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

const upstreamPreservationBytes = 128 << 20

// RetainedUpstreamSpecs discovers installations, not running containers. Callers
// must resolve the successor recipe's bindings with saved settings before Prepare.
func RetainedUpstreamSpecs(rt Runtime, sourceURL, sourcePath string) ([]ContainerSpec, error) {
	r, ok := rt.(*upstreamRuntime)
	if !ok {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []ContainerSpec
	seen := make(map[string]bool)
	ids := make([]string, 0, len(r.records))
	for id := range r.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := r.records[id]
		if rec.Spec.Upstream.SourceURL != sourceURL || filepath.Clean(rec.Spec.Upstream.SourcePath) != filepath.Clean(sourcePath) {
			continue
		}
		identitySpec := rec.Spec
		identitySource := *rec.Spec.Upstream
		identitySource.Revision = ""
		identitySpec.Upstream = &identitySource
		identity := r.workspace(&identitySpec)
		if seen[identity] {
			continue
		}
		selected, err := r.sourcePredecessor(&identitySpec)
		if err != nil {
			return nil, err
		}
		if selected == nil {
			continue
		}
		rec = selected
		if err := r.verifyWorkspace(rec); err != nil {
			return nil, err
		}
		// Deep copy so resolving new bindings cannot mutate retained authority.
		var spec ContainerSpec
		data, err := json.Marshal(rec.Spec)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &spec); err != nil {
			return nil, err
		}
		result = append(result, spec)
		seen[identity] = true
	}
	return result, nil
}

// PrepareUpstreamSource carries installation-local source changes to a resolved
// successor without inspecting, stopping, starting, or replacing any container.
func PrepareUpstreamSource(ctx context.Context, rt Runtime, spec *ContainerSpec) error {
	r, ok := rt.(*upstreamRuntime)
	if !ok || !r.allowExecution {
		return fmt.Errorf("upstream.execution_disabled")
	}
	if spec == nil || spec.Upstream == nil {
		return fmt.Errorf("upstream.source_spec_required")
	}
	if err := validateUpstream(spec); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	workspace := r.reusableWorkspace(spec)
	if _, err := os.Lstat(filepath.Join(workspace, "repository")); err == nil {
		return prepareUpstreamWorkspace(ctx, spec, workspace, nil, io.Discard, io.Discard)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	previous, err := r.sourcePredecessor(spec)
	if err != nil {
		return err
	}
	return prepareUpstreamWorkspace(ctx, spec, workspace, previous, io.Discard, io.Discard)
}

func sameUpstreamInstallation(a, b *ContainerSpec) bool {
	if a.Upstream.SourceURL != b.Upstream.SourceURL || filepath.Clean(a.Upstream.SourcePath) != filepath.Clean(b.Upstream.SourcePath) || a.Labels[LabelRank] != b.Labels[LabelRank] {
		return false
	}
	names := func(spec *ContainerSpec) []string {
		var result []string
		for name := range upstreamContainerNames(spec.Upstream) {
			result = append(result, name)
		}
		sort.Strings(result)
		return result
	}
	return reflect.DeepEqual(names(a), names(b))
}

func upstreamSourceRecorded(workspace string) (bool, error) {
	info, err := os.Lstat(filepath.Join(workspace, "source-state.json"))
	if err == nil {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("upstream.source_snapshot_invalid")
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, marker := range []string{"repository", "installed.json"} {
		if _, err := os.Lstat(filepath.Join(workspace, marker)); err == nil {
			return false, fmt.Errorf("upstream.source_snapshot_missing: %s", workspace)
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (r *upstreamRuntime) sourcePredecessor(spec *ContainerSpec) (*upstreamRecord, error) {
	current, historical := make(map[string]*upstreamRecord), make(map[string]*upstreamRecord)
	ids := make([]string, 0, len(r.records))
	for id := range r.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := r.records[id]
		if !sameUpstreamInstallation(&rec.Spec, spec) || strings.EqualFold(rec.Spec.Upstream.Revision, spec.Upstream.Revision) {
			continue
		}
		if err := r.verifyWorkspace(rec); err != nil {
			return nil, err
		}
		recorded, err := upstreamSourceRecorded(rec.Workspace)
		if err != nil {
			return nil, err
		}
		if !recorded {
			continue
		}
		if !rec.Superseded && !rec.Removed {
			current[rec.Workspace] = rec
		}
		historical[rec.Workspace] = rec
	}
	candidates := current
	if len(candidates) == 0 {
		candidates = historical
	}
	if len(candidates) > 1 {
		return nil, fmt.Errorf("upstream.source_predecessor_ambiguous")
	}
	for _, rec := range candidates {
		if rec.Job != nil && upstreamProcessAlive(rec.Job.PID, rec.Job.ProcessIdentity) {
			return nil, fmt.Errorf("upstream.source_predecessor_busy")
		}
		return rec, nil
	}
	return nil, nil
}

func prepareUpstreamWorkspace(ctx context.Context, spec *ContainerSpec, workspace string, previous *upstreamRecord, stdout, stderr io.Writer) error {
	if info, err := os.Lstat(workspace); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("upstream.workspace_invalid")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	repository := filepath.Join(workspace, "repository")
	if _, err := os.Lstat(repository); err == nil {
		// Never reconfigure an existing (possibly running) installation here.
		return verifyUpstreamSource(ctx, spec, repository)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(workspace), 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(workspace)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	identity := workspace
	for _, entry := range entries {
		if entry.Name() != "source-identity.json" {
			return fmt.Errorf("upstream.source_missing: retained installation checkout has disappeared")
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("upstream.workspace_identity_invalid")
		}
		if err := readUpstreamJSON(filepath.Join(workspace, entry.Name()), &identity); err != nil {
			return err
		}
	}
	if previous != nil {
		if !sameUpstreamInstallation(&previous.Spec, spec) || previous.Workspace == workspace {
			return fmt.Errorf("upstream.source_predecessor_invalid")
		}
		if err := verifyUpstreamSource(ctx, &previous.Spec, filepath.Join(previous.Workspace, "repository")); err != nil {
			return err
		}
	}
	staging, err := os.MkdirTemp(filepath.Dir(workspace), ".prepare-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	stagedRepository := filepath.Join(staging, "repository")
	if err := materializeUpstream(ctx, spec, stagedRepository, stdout, stderr); err != nil {
		return err
	}
	if previous != nil {
		if err := carryUpstreamSource(ctx, previous, spec, stagedRepository); err != nil {
			return err
		}
	} else if err := recordUpstreamSource(ctx, spec, stagedRepository); err != nil {
		return err
	}
	if err := configureUpstreamSource(ctx, spec, stagedRepository); err != nil {
		return err
	}
	working, err := upstreamPath(stagedRepository, spec.Upstream.SourcePath, false)
	if err != nil {
		return err
	}
	if err := configureUpstreamEnv(spec, working, filepath.Join(staging, "env-state.json")); err != nil {
		return err
	}
	if err := recordUpstreamSource(ctx, spec, stagedRepository); err != nil {
		return err
	}
	if previous != nil {
		// Catch modifications during preparation; never bless a moving source.
		if err := verifyUpstreamSource(ctx, &previous.Spec, filepath.Join(previous.Workspace, "repository")); err != nil {
			return err
		}
	}
	if err := writeUpstreamJSON(filepath.Join(staging, "source-identity.json"), identity); err != nil {
		return err
	}
	// An unmaterialized Create may have reserved the directory with only its
	// identity marker. No installed workspace or cache directory is removed.
	if len(entries) != 0 {
		if err := os.Remove(filepath.Join(workspace, "source-identity.json")); err != nil {
			return err
		}
	}
	if err := os.Remove(workspace); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(staging, workspace)
}

type upstreamSourceFile struct {
	Data   []byte
	Mode   os.FileMode
	Exists bool
}

func upstreamWorkingFile(repository, relative string) (upstreamSourceFile, error) {
	path, err := upstreamSourcePath(repository, relative)
	if err != nil {
		return upstreamSourceFile{}, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return upstreamSourceFile{}, nil
	}
	if err != nil {
		return upstreamSourceFile{}, err
	}
	file := upstreamSourceFile{Mode: info.Mode(), Exists: true}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		file.Data = []byte(target)
		return file, err
	}
	if !info.Mode().IsRegular() || info.Mode()&^(os.ModePerm) != 0 || info.Size() > sourceconfig.MaxSourceBytes {
		return file, fmt.Errorf("upstream.preservation_unsupported_file: %s", relative)
	}
	f, err := os.Open(path)
	if err != nil {
		return file, err
	}
	defer f.Close()
	file.Data, err = io.ReadAll(io.LimitReader(f, sourceconfig.MaxSourceBytes+1))
	if len(file.Data) > sourceconfig.MaxSourceBytes {
		return file, fmt.Errorf("upstream.preservation_file_too_large: %s", relative)
	}
	return file, err
}

func upstreamOriginalFile(ctx context.Context, repository, relative string) (upstreamSourceFile, error) {
	if _, err := upstreamSourcePath(repository, relative); err != nil {
		return upstreamSourceFile{}, err
	}
	// A nested submodule has its own immutable origin, not a root-tree blob.
	root := repository
	parts := strings.Split(relative, "/")
	for i := 1; i < len(parts); i++ {
		dir := filepath.Join(repository, filepath.Join(parts[:i]...))
		if info, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return upstreamSourceFile{}, fmt.Errorf("upstream.source_git_symlink")
			}
			root = dir
		} else if !errors.Is(err, os.ErrNotExist) {
			return upstreamSourceFile{}, err
		}
	}
	name, err := filepath.Rel(root, filepath.Join(repository, relative))
	if err != nil {
		return upstreamSourceFile{}, err
	}
	entry, err := upstreamGitOutput(ctx, root, upstreamInventoryLimit, "ls-tree", "-z", "HEAD", "--", filepath.ToSlash(name))
	if err != nil || len(entry) == 0 {
		return upstreamSourceFile{}, err
	}
	metadata, path, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
	fields := strings.Fields(metadata)
	if !ok || path != filepath.ToSlash(name) || len(fields) != 3 || fields[1] != "blob" {
		return upstreamSourceFile{}, fmt.Errorf("upstream.preservation_unsupported_tree: %s", relative)
	}
	file := upstreamSourceFile{Exists: true}
	switch fields[0] {
	case "100644":
		file.Mode = 0644
	case "100755":
		file.Mode = 0755
	case "120000":
		file.Mode = os.ModeSymlink | 0777
	default:
		return file, fmt.Errorf("upstream.preservation_unsupported_mode: %s", relative)
	}
	file.Data, err = upstreamGitOutput(ctx, root, sourceconfig.MaxSourceBytes, "cat-file", "blob", fields[2])
	return file, err
}

func upstreamPreservationPaths(ctx context.Context, repository string, authenticated map[string]string) ([]string, error) {
	changed, err := upstreamGitOutput(ctx, repository, upstreamInventoryLimit, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", "HEAD")
	if err != nil {
		return nil, err
	}
	candidates := make(map[string]bool)
	for _, path := range strings.Split(string(changed), "\x00") {
		if _, ok := authenticated[path]; ok {
			candidates[path] = true
		}
	}
	tree, err := upstreamGitOutput(ctx, repository, upstreamInventoryLimit, "ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return nil, err
	}
	modes := make(map[string]os.FileMode)
	for _, entry := range strings.Split(string(tree), "\x00") {
		metadata, path, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		switch strings.Fields(metadata)[0] {
		case "100644":
			modes[path] = 0644
		case "100755":
			modes[path] = 0755
		case "120000":
			modes[path] = os.ModeSymlink | 0777
		case "160000":
			for _, modified := range strings.Split(string(changed), "\x00") {
				if modified == path {
					return nil, fmt.Errorf("upstream.preservation_modified_submodule: %s", path)
				}
			}
		}
	}
	for relative := range authenticated {
		mode, tracked := modes[relative]
		if !tracked {
			candidates[relative] = true
			continue
		}
		path, err := upstreamSourcePath(repository, relative)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			candidates[relative] = true
		} else if err != nil {
			return nil, err
		} else if info.Mode() != mode {
			candidates[relative] = true
		}
	}
	paths := make([]string, 0, len(candidates))
	for path := range candidates {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func carryUpstreamSource(ctx context.Context, previous *upstreamRecord, spec *ContainerSpec, repository string) error {
	oldRepository := filepath.Join(previous.Workspace, "repository")
	var authenticated map[string]string
	if err := readUpstreamJSON(filepath.Join(previous.Workspace, "source-state.json"), &authenticated); err != nil {
		return err
	}
	configuration := make(map[string]upstreamConfigurationState)
	if err := readUpstreamJSON(filepath.Join(previous.Workspace, "configuration-state.json"), &configuration); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	paths, err := upstreamPreservationPaths(ctx, oldRepository, authenticated)
	if err != nil {
		return err
	}
	type change struct {
		relative string
		file     upstreamSourceFile
	}
	var changes []change
	total := 0
	for _, relative := range paths {
		original, err := upstreamOriginalFile(ctx, oldRepository, relative)
		if err != nil {
			return err
		}
		local, err := upstreamWorkingFile(oldRepository, relative)
		if err != nil {
			return err
		}
		if prior, configured := configuration[relative]; configured {
			applied, err := upstreamConfiguredBaseline(ctx, &previous.Spec, oldRepository, relative, original.Data, local.Data, prior)
			if err != nil {
				return err
			}
			local.Data, err = sourceconfig.MergeLocalChanges(ctx, filepath.Dir(repository), applied, local.Data, original.Data)
			if err != nil {
				return err
			}
		}
		generatedEnv := false
		if previous.Spec.Upstream.EnvFile != "" && relative == filepath.ToSlash(filepath.Join(previous.Spec.Upstream.SourcePath, previous.Spec.Upstream.EnvFile)) && local.Exists {
			withoutOverrides := previous.Spec
			withoutOverrides.Env = nil
			lines, _, err := renderUpstreamEnv(&withoutOverrides, strings.Split(strings.TrimSuffix(string(local.Data), "\n"), "\n"), filepath.Join(previous.Workspace, "env-state.json"))
			if err != nil {
				return err
			}
			local.Data = []byte(strings.Join(lines, "\n") + "\n")
			generatedEnv = !original.Exists
			if generatedEnv && previous.Spec.Upstream.EnvTemplate != "" {
				original, err = upstreamOriginalFile(ctx, oldRepository, filepath.ToSlash(filepath.Join(previous.Spec.Upstream.SourcePath, previous.Spec.Upstream.EnvTemplate)))
				if err != nil {
					return err
				}
				if !original.Exists || !original.Mode.IsRegular() {
					return fmt.Errorf("upstream.preservation_env_template_invalid")
				}
				original.Mode = 0600
			}
		}
		if reflect.DeepEqual(original, local) {
			continue
		}
		incoming, err := upstreamWorkingFile(repository, relative)
		if err != nil {
			return err
		}
		if generatedEnv && !incoming.Exists && spec.Upstream.EnvFile == previous.Spec.Upstream.EnvFile && spec.Upstream.EnvTemplate != "" {
			incoming, err = upstreamWorkingFile(repository, filepath.ToSlash(filepath.Join(spec.Upstream.SourcePath, spec.Upstream.EnvTemplate)))
			if err != nil {
				return err
			}
			if !incoming.Exists || !incoming.Mode.IsRegular() {
				return fmt.Errorf("upstream.preservation_env_template_invalid")
			}
			incoming.Mode = 0600
		}
		result := local
		if local.Exists && incoming.Exists && local.Mode.IsRegular() && incoming.Mode.IsRegular() && (!original.Exists || original.Mode.IsRegular()) {
			result.Data, err = sourceconfig.MergeLocalChanges(ctx, filepath.Dir(repository), original.Data, local.Data, incoming.Data)
			if err != nil {
				return fmt.Errorf("%s: %w", relative, err)
			}
			if local.Mode == original.Mode {
				result.Mode = incoming.Mode
			}
		}
		if result.Exists && result.Mode&os.ModeSymlink != 0 {
			target := string(result.Data)
			if filepath.IsAbs(target) || strings.ContainsAny(target, "\\\x00\r\n") {
				return fmt.Errorf("upstream.preservation_symlink_escapes: %s", relative)
			}
			// Dot-dot after another symlink has platform-specific traversal
			// semantics. Refuse it rather than authorizing a cleaned alias.
			for _, part := range strings.Split(target, "/") {
				if part == ".." {
					return fmt.Errorf("upstream.preservation_symlink_parent_traversal: %s", relative)
				}
			}
			if _, err := upstreamPath(repository, filepath.ToSlash(filepath.Join(filepath.Dir(relative), target)), true); err != nil {
				return err
			}
		}
		total += len(original.Data) + len(local.Data) + len(incoming.Data) + len(result.Data)
		if total > upstreamPreservationBytes || len(changes) >= 4096 {
			return fmt.Errorf("upstream.preservation_limit")
		}
		changes = append(changes, change{relative, result})
	}
	// Everything was read and merged before the first checkout write. Each path
	// is checked again because an earlier preserved link could affect a parent.
	for _, change := range changes {
		path, err := upstreamSourcePath(repository, change.relative)
		if err != nil {
			return err
		}
		if !change.file.Exists {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if change.file.Mode&os.ModeSymlink != 0 {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.Symlink(string(change.file.Data), path); err != nil {
				return err
			}
		} else if err := replaceConfigurationFile(path, change.file.Data, change.file.Mode.Perm()); err != nil {
			return err
		}
	}
	return recordUpstreamSource(ctx, spec, repository, paths...)
}
