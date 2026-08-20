package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
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
	opts := image.PullOptions{}
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
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("image pull %s: %w", spec.Reference, err)
	}
	return nil
}

// encodeRegistryAuth produces the base64 authconfig blob the Engine expects
// in ImagePullOptions.RegistryAuth.
func encodeRegistryAuth(auth *Auth) (string, error) {
	return registry.EncodeAuthConfig(registry.AuthConfig{
		Username: auth.Username,
		Password: auth.Password,
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
	ci := &ContainerInfo{ID: info.ID, Name: strings.TrimPrefix(info.Name, "/"), State: info.State.Status, Labels: info.Config.Labels}
	if info.State.ExitCode != 0 {
		ci.ExitCode = info.State.ExitCode
		ci.Error = info.State.Error
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
