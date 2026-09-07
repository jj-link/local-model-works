package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/jj-link/local-model-works/internal/downloads"
)

// dockerRuntime implements Runtime over the Docker Engine API.
type dockerRuntime struct {
	cli *client.Client
}

// NewDocker returns a runtime bound to the Docker socket at socketPath.
func NewDocker(socketPath string) (Runtime, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+socketPath),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &dockerRuntime{cli: cli}, nil
}

func (r *dockerRuntime) Ping(ctx context.Context) (string, error) {
	v, err := r.cli.ServerVersion(ctx)
	if err != nil {
		return "", fmt.Errorf("docker ping: %w", err)
	}
	return v.Version, nil
}

func (r *dockerRuntime) Pull(ctx context.Context, spec *PullSpec) error {
	opts := image.PullOptions{Platform: spec.Platform}
	if spec.Auth != nil {
		enc, err := encodeRegistryAuth(spec.Auth)
		if err != nil {
			return fmt.Errorf("registry auth: %w", err)
		}
		opts.RegistryAuth = enc
	}
	rc, err := r.cli.ImagePull(ctx, spec.Reference, opts)
	if err != nil {
		return fmt.Errorf("image pull %s: %w", spec.Reference, err)
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg jsonmessage.JSONMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return fmt.Errorf("image pull %s: undecodable stream message: %w", spec.Reference, err)
		}
		if msg.Error != nil && msg.Error.Message != "" {
			return fmt.Errorf("image pull %s: %s", spec.Reference, msg.Error.Message)
		}
		if msg.ErrorMessage != "" {
			return fmt.Errorf("image pull %s: %s", spec.Reference, msg.ErrorMessage)
		}
		if spec.Progress != nil {
			p := ImagePullProgress{Layer: msg.ID, Phase: msg.Status}
			if msg.Progress != nil {
				p.BytesDone, p.BytesTotal = msg.Progress.Current, msg.Progress.Total
			}
			spec.Progress(p)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("image pull %s: %w", spec.Reference, err)
	}
	return nil
}

func (r *dockerRuntime) InspectImage(ctx context.Context, reference, platform string) (*ImageInfo, error) {
	if !IsContentAddressedImage(reference) {
		return nil, fmt.Errorf("image.reference_unpinned")
	}
	info, err := r.cli.ImageInspect(ctx, reference)
	if err != nil {
		return nil, err
	}
	actualPlatform := downloads.CanonicalImagePlatform(info.Os, info.Architecture, info.Variant)
	if platform != "" && actualPlatform != platform {
		return nil, fmt.Errorf("image.platform_mismatch: have %s, need %s", actualPlatform, platform)
	}
	digest := reference
	if _, pinned, ok := strings.Cut(reference, "@"); ok {
		digest = pinned
		found := false
		for _, repoDigest := range info.RepoDigests {
			if _, actual, ok := strings.Cut(repoDigest, "@"); ok && actual == pinned && imageRepository(repoDigest) == imageRepository(reference) {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("image.digest_mismatch")
		}
	} else if info.ID != digest {
		return nil, fmt.Errorf("image.digest_mismatch")
	}
	result := &ImageInfo{Reference: reference, Digest: digest, Platform: actualPlatform, SizeBytes: info.Size}
	if info.Descriptor != nil {
		if info.Descriptor.Digest.String() != digest {
			return nil, fmt.Errorf("image.descriptor_mismatch")
		}
		switch info.Descriptor.MediaType {
		case "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json":
			result.IndexDigest = digest
			result.ManifestDigest = digest
		case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
			detailed, err := r.cli.ImageInspect(ctx, reference, client.ImageInspectWithManifests(true))
			if err != nil {
				return nil, fmt.Errorf("image.manifest_inspection_unavailable: %w", err)
			}
			for _, manifest := range detailed.Manifests {
				if !manifest.Available || manifest.Kind != image.ManifestKindImage || manifest.ImageData == nil {
					continue
				}
				p := manifest.ImageData.Platform
				selected := downloads.CanonicalImagePlatform(p.OS, p.Architecture, p.Variant)
				if selected != actualPlatform {
					continue
				}
				if result.ManifestDigest != "" && result.ManifestDigest != manifest.ID {
					return nil, fmt.Errorf("image.platform_ambiguous")
				}
				if !IsSHA256Digest(manifest.ID) || manifest.Descriptor.Digest.String() != manifest.ID {
					return nil, fmt.Errorf("image.manifest_digest_invalid")
				}
				result.IndexDigest = digest
				result.ManifestDigest = manifest.ID
				result.SizeBytes = manifest.Size.Total
			}
			if result.ManifestDigest == "" {
				return nil, fmt.Errorf("image.platform_content_missing")
			}
		}
	}
	return result, nil
}

func imageRepository(reference string) string {
	name, _, _ := strings.Cut(reference, "@")
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	first, _, hasSlash := strings.Cut(name, "/")
	if !hasSlash {
		return "docker.io/library/" + name
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io/" + name
	}
	if first == "index.docker.io" || first == "registry-1.docker.io" {
		return "docker.io" + name[len(first):]
	}
	return name
}

func (r *dockerRuntime) ImageStorage(ctx context.Context) (*ImageStorageInfo, error) {
	info, err := r.cli.Info(ctx)
	if err != nil {
		return nil, err
	}
	if info.DockerRootDir == "" {
		return nil, fmt.Errorf("image.storage_root_unavailable")
	}
	if stat, err := os.Stat(info.DockerRootDir); err != nil || !stat.IsDir() {
		return nil, fmt.Errorf("image.storage_root_unavailable")
	}
	return InspectStorage(info.DockerRootDir)
}

// PrepareHost uses a controller-owned, digest-pinned helper with no network to
// apply the bounded memory controls encoded in the managed container spec.
// The imported recipe never supplies helper code, image, paths, or privileges.
func (r *dockerRuntime) PrepareHost(ctx context.Context, spec *ContainerSpec) error {
	if err := ValidateManagedSpec(spec); err != nil {
		return err
	}
	preparation := spec.HostPreparation
	if preparation == nil {
		return nil
	}
	script, err := hostPreparationScript(preparation)
	if err != nil {
		return err
	}
	helperImage := downloads.HostPreparationImage
	for _, resource := range spec.AcquisitionResources {
		if resource.Source.Reference+"@"+resource.IndexDigest == downloads.HostPreparationImage {
			helperImage = resource.Source.Reference + "@" + resource.ManifestDigest
		}
	}
	if spec.AcquisitionPolicy == "require-existing" {
		if _, err := r.InspectImage(ctx, helperImage, spec.ImagePlatform); err != nil {
			return fmt.Errorf("host.prepare_helper_recheck_required: %w", err)
		}
	} else if err := r.Pull(ctx, &PullSpec{Reference: helperImage, Platform: spec.ImagePlatform}); err != nil {
		return fmt.Errorf("host.prepare_helper_pull: %w", err)
	}

	name := spec.Name + "-host-prepare"
	labels := make(map[string]string, len(spec.Labels)+1)
	for key, value := range spec.Labels {
		labels[key] = value
	}
	labels[LabelModule] = HostPreparationModule
	if existing, inspectErr := r.cli.ContainerInspect(ctx, name); inspectErr == nil {
		for _, key := range []string{LabelManaged, LabelDeployment, LabelRun, LabelRank, LabelRecipe} {
			if existing.Config.Labels[key] != labels[key] {
				return fmt.Errorf("host.prepare_identity_mismatch: existing helper %s is not owned by this run", name)
			}
		}
		if removeErr := r.cli.ContainerRemove(ctx, existing.ID, container.RemoveOptions{Force: true}); removeErr != nil {
			return fmt.Errorf("host.prepare_cleanup: %w", removeErr)
		}
	} else if !errdefs.IsNotFound(inspectErr) {
		return fmt.Errorf("host.prepare_inspect: %w", inspectErr)
	}

	created, err := r.cli.ContainerCreate(
		ctx,
		&container.Config{Image: helperImage, Cmd: []string{"sh", "-ec", script}, Labels: labels},
		&container.HostConfig{
			Privileged:     true,
			ReadonlyRootfs: true,
			NetworkMode:    "none",
			RestartPolicy:  container.RestartPolicy{Name: "no"},
		},
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}},
		nil,
		name,
	)
	if err != nil {
		return fmt.Errorf("host.prepare_create: %w", err)
	}
	remove := func() {
		_ = r.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
	}
	if err := r.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		remove()
		return fmt.Errorf("host.prepare_start: %w", err)
	}
	statusCh, errCh := r.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	var statusCode int64
	select {
	case waitErr := <-errCh:
		remove()
		if waitErr == nil {
			waitErr = fmt.Errorf("wait channel closed")
		}
		return fmt.Errorf("host.prepare_wait: %w", waitErr)
	case status := <-statusCh:
		statusCode = status.StatusCode
	}
	output := r.containerOutput(ctx, created.ID)
	remove()
	if statusCode != 0 {
		if output == "" {
			output = "helper exited without diagnostics"
		}
		return fmt.Errorf("host.prepare_failed: exit %d: %s", statusCode, output)
	}
	return nil
}

func hostPreparationScript(spec *HostPreparationSpec) (string, error) {
	if spec == nil {
		return "", fmt.Errorf("host.prepare_missing")
	}
	if spec.Swappiness != nil && (*spec.Swappiness < 0 || *spec.Swappiness > 200) {
		return "", fmt.Errorf("host.prepare_swappiness_invalid: %d", *spec.Swappiness)
	}
	lines := []string{"set -eu"}
	if spec.RequireSwap {
		lines = append(lines,
			`swap_kb="$(awk '/^SwapTotal:/ {print $2; exit}' /proc/meminfo)"`,
			`test "${swap_kb:-0}" -gt 0 || { echo "swap is disabled; enable swap before launching this recipe" >&2; exit 40; }`,
		)
	}
	if spec.Swappiness != nil {
		lines = append(lines, fmt.Sprintf(`printf '%%s\n' %d > /proc/sys/vm/swappiness`, *spec.Swappiness))
	}
	if spec.DropPageCache {
		lines = append(lines, "sync", `printf '3\n' > /proc/sys/vm/drop_caches`)
	}
	if spec.Swappiness != nil {
		lines = append(lines,
			fmt.Sprintf(`test "$(cat /proc/sys/vm/swappiness)" -eq %d || { echo "vm.swappiness verification failed" >&2; exit 41; }`, *spec.Swappiness),
		)
	}
	return strings.Join(lines, "\n"), nil
}

func (r *dockerRuntime) containerOutput(ctx context.Context, id string) string {
	rc, err := r.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ""
	}
	defer rc.Close()
	var output bytes.Buffer
	if _, err := stdcopy.StdCopy(&output, &output, rc); err != nil {
		return ""
	}
	return strings.TrimSpace(output.String())
}

// encodeRegistryAuth produces the base64 authconfig blob the Engine expects
// in ImagePullOptions.RegistryAuth.
func encodeRegistryAuth(auth *Auth) (string, error) {
	return registry.EncodeAuthConfig(registry.AuthConfig{
		Username:      auth.Username,
		Password:      auth.Password,
		RegistryToken: auth.RegistryToken,
		ServerAddress: auth.ServerAddress,
	})
}

func (r *dockerRuntime) Create(ctx context.Context, spec *ContainerSpec) (string, error) {
	if err := ValidateManagedSpec(spec); err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image:      ImageRef(spec),
		Hostname:   spec.Name,
		Env:        spec.Env,
		Cmd:        spec.Cmd,
		Entrypoint: spec.Entrypoint,
		Labels:     spec.Labels,
		WorkingDir: spec.WorkingDir,
	}
	hostCfg := container.HostConfig{
		NetworkMode:    container.NetworkMode(spec.NetworkMode),
		ReadonlyRootfs: spec.ReadonlyRootfs,
		CapDrop:        capDrops(spec.CapDrop),
		ShmSize:        spec.ShmBytes,
		RestartPolicy:  container.RestartPolicy{Name: "no"},
	}
	if spec.TmpfsBytes > 0 {
		hostCfg.Tmpfs = map[string]string{
			"/tmp": fmt.Sprintf("rw,exec,nosuid,nodev,size=%d", spec.TmpfsBytes),
			"/run": "rw,nosuid,nodev,size=67108864",
		}
	}
	if spec.NoNewPrivileges {
		hostCfg.SecurityOpt = append(hostCfg.SecurityOpt, "no-new-privileges:true")
	}
	if spec.MemoryBytes > 0 {
		hostCfg.Resources.Memory = spec.MemoryBytes
	}
	if spec.CPU > 0 {
		hostCfg.Resources.NanoCPUs = int64(spec.CPU * 1e9)
	}
	if spec.PidsLimit > 0 {
		lim := int64(spec.PidsLimit)
		hostCfg.PidsLimit = &lim
	}
	for _, u := range spec.Ulimits {
		hostCfg.Ulimits = append(hostCfg.Ulimits, &container.Ulimit{
			Name: u.Name,
			Hard: u.Hard,
			Soft: u.Soft,
		})
	}
	for _, m := range spec.Mounts {
		hostCfg.Mounts = append(hostCfg.Mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Dest,
			ReadOnly: m.ReadOnly,
		})
	}
	if spec.GPUsAll {
		hostCfg.DeviceRequests = append(hostCfg.DeviceRequests, container.DeviceRequest{Driver: "nvidia", Count: -1})
	} else if len(spec.GPUDeviceIDs) > 0 {
		hostCfg.DeviceRequests = append(hostCfg.DeviceRequests, container.DeviceRequest{Driver: "nvidia", DeviceIDs: spec.GPUDeviceIDs})
	}
	for _, p := range spec.RDMAPaths {
		hostCfg.Devices = append(hostCfg.Devices, container.DeviceMapping{PathOnHost: p, PathInContainer: p, CgroupPermissions: "rwm"})
	}
	if spec.NetworkMode == "bridge" || spec.NetworkMode == "" {
		if len(spec.Ports) > 0 {
			ports := nat.PortMap{}
			cfg.ExposedPorts = nat.PortSet{}
			for _, p := range spec.Ports {
				proto := p.Protocol
				if proto == "" {
					proto = "tcp"
				}
				key := nat.Port(fmt.Sprintf("%d/%s", p.Container, proto))
				cfg.ExposedPorts[key] = struct{}{}
				if p.Host > 0 {
					ports[key] = []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: strconv.Itoa(p.Host)}}
				}
			}
			hostCfg.PortBindings = ports
		}
	}

	netCfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	resp, err := r.cli.ContainerCreate(ctx, cfg, &hostCfg, netCfg, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("container create %s: %w", spec.Name, err)
	}
	return resp.ID, nil
}

func (r *dockerRuntime) Start(ctx context.Context, id string) error {
	return r.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (r *dockerRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	opts := container.StopOptions{}
	if timeoutSeconds > 0 {
		t := timeoutSeconds
		opts.Timeout = &t
	}
	return r.cli.ContainerStop(ctx, id, opts)
}

func (r *dockerRuntime) Remove(ctx context.Context, id string, force bool) error {
	return r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
}

func (r *dockerRuntime) Inspect(ctx context.Context, idOrName string) (*ContainerInfo, error) {
	info, err := r.cli.ContainerInspect(ctx, idOrName)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", idOrName, err)
	}
	ci := &ContainerInfo{
		ID:        info.ID,
		Name:      strings.TrimPrefix(info.Name, "/"),
		State:     info.State.Status,
		Status:    info.State.Status,
		ExitCode:  info.State.ExitCode,
		Error:     info.State.Error,
		OOMKilled: info.State.OOMKilled,
		Labels:    info.Config.Labels,
	}
	for _, p := range info.NetworkSettings.Ports {
		for _, pb := range p {
			if n, err := strconv.Atoi(pb.HostPort); err == nil {
				ci.Ports = append(ci.Ports, n)
			}
		}
	}
	return ci, nil
}

func (r *dockerRuntime) ListByLabel(ctx context.Context, key, value string) ([]ContainerInfo, error) {
	f := filters.NewArgs()
	f.Add("label", key+"="+value)
	containers, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	out := make([]ContainerInfo, 0, len(containers))
	for _, c := range containers {
		state := c.State
		if idx := strings.IndexByte(state, ' '); idx > 0 {
			state = state[:idx]
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, ContainerInfo{ID: c.ID, Name: name, State: state, Labels: c.Labels})
	}
	return out, nil
}

// LogsFollow streams container output (stdout and stderr interleaved);
// the caller owns closing rc.
func (r *dockerRuntime) LogsFollow(ctx context.Context, id string, fromStdout, fromStderr bool) (io.ReadCloser, error) {
	rc, err := r.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: fromStdout,
		ShowStderr: fromStderr,
		Follow:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("logs %s: %w", id, err)
	}
	pr, pw := io.Pipe()
	go func() {
		defer rc.Close()
		defer pw.Close()
		_, _ = stdcopy.StdCopy(pw, pw, rc)
	}()
	return pr, nil
}

// LogsStreams streams container output with stdout and stderr separated
// into two pipes; the caller owns closing both.
func (r *dockerRuntime) LogsStreams(ctx context.Context, id string) (stdout, stderr io.ReadCloser, err error) {
	rc, err := r.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("logs %s: %w", id, err)
	}
	prOut, pwOut := io.Pipe()
	prErr, pwErr := io.Pipe()
	go func() {
		defer rc.Close()
		defer pwOut.Close()
		defer pwErr.Close()
		_, _ = stdcopy.StdCopy(pwOut, pwErr, rc)
	}()
	return prOut, prErr, nil
}

// IsSHA256Digest reports whether value is a complete lowercase SHA-256 digest.
func IsSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, c := range value[len("sha256:"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// IsContentAddressedImage reports whether ref is a digest-pinned registry
// reference or a bare image ID digest.
func IsContentAddressedImage(ref string) bool {
	if IsSHA256Digest(ref) {
		return true
	}
	name, digest, ok := strings.Cut(ref, "@")
	return ok && name != "" && IsSHA256Digest(digest)
}

// imageRef resolves the pull/create reference to a digest-qualified form
// when a pinned digest is present, so a moving tag cannot change the image
// between validation and launch.
func ImageRef(spec *ContainerSpec) string {
	if spec.ImageDigest == "" {
		return spec.Image
	}
	ref := spec.Image
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	return ref + "@" + spec.ImageDigest
}

// capDrops defaults to dropping ALL capabilities; recipes that need a
// specific set provide it explicitly.
func capDrops(list []string) []string {
	if len(list) == 0 {
		return []string{"ALL"}
	}
	return list
}
