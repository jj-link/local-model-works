// Package runtime defines the container-runtime contract used by agents to
// execute recipe workloads. The Docker Engine implementation is the first
// and, for this release, only implementation; specs are typed so the
// runtime is swappable without touching scheduling code.
package runtime

import (
	"context"
	"fmt"
	"io"
	"strconv"
)

// MountSpec is one explicit bind mount.
type MountSpec struct {
	Source   string `json:"source"`
	Dest     string `json:"dest"`
	ReadOnly bool   `json:"readOnly"`
}

// PortSpec is one published port (bridge mode only).
type PortSpec struct {
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol"`
}

// Ulimit is one rlimit. Soft/Hard are rlimit values; -1 means unlimited.
type Ulimit struct {
	Name string `json:"name"`
	Hard int64  `json:"hard"`
	Soft int64  `json:"soft"`
}

// HostPreparationSpec is the bounded set of host-memory controls a reviewed
// recipe may request. It intentionally carries no command, image, or path.
type HostPreparationSpec struct {
	RequireSwap   bool `json:"requireSwap,omitempty"`
	Swappiness    *int `json:"swappiness,omitempty"`
	DropPageCache bool `json:"dropPageCache,omitempty"`
}

// ContainerSpec is the typed, JSON-stable workload description sent to an
// agent inside a WorkloadCommand.
type ContainerSpec struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	ImageDigest     string            `json:"imageDigest,omitempty"`
	Entrypoint      []string          `json:"entrypoint,omitempty"`
	Cmd             []string          `json:"cmd"`
	Env             []string          `json:"env,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	WorkingDir      string            `json:"workingDir,omitempty"`
	NetworkMode     string            `json:"networkMode"` // bridge | host | none
	Ports           []PortSpec        `json:"ports,omitempty"`
	ReadonlyRootfs  bool              `json:"readonlyRootfs"`
	NoNewPrivileges bool              `json:"noNewPrivileges"`
	CapDrop         []string          `json:"capDrop,omitempty"`
	ShmBytes        int64             `json:"shmBytes,omitempty"`
	TmpfsBytes      int64             `json:"tmpfsBytes,omitempty"`
	PidsLimit       int               `json:"pidsLimit,omitempty"`
	MemoryBytes     int64             `json:"memoryBytes,omitempty"`
	CPU             float64           `json:"cpu,omitempty"`
	CPUSetCpus      string            `json:"cpusetCpus,omitempty"`
	Mounts          []MountSpec       `json:"mounts,omitempty"`
	GPUDeviceIDs    []string          `json:"gpuDeviceIDs,omitempty"`
	GPUsAll         bool              `json:"gpusAll,omitempty"`
	RDMAPaths       []string          `json:"rdmaPaths,omitempty"`
	// Ulimits carries per-resource rlimits (e.g. memlock, stack). Required for
	// RoCE/NCCL workloads: the container drops all capabilities, so the default
	// RLIMIT_MEMLOCK (8 KiB) makes ibv_reg_mr_iova2 fail with ENOMEM.
	Ulimits         []Ulimit             `json:"ulimits,omitempty"`
	HostPreparation *HostPreparationSpec `json:"hostPreparation,omitempty"`
}

// ContainerInfo is the observed container state.
type ContainerInfo struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	State     string            `json:"state"` // created|running|exited|paused|removing|dead
	Status    string            `json:"status"`
	ExitCode  int               `json:"exitCode,omitempty"`
	Error     string            `json:"error,omitempty"`
	OOMKilled bool              `json:"oomKilled,omitempty"`
	Ports     []int             `json:"ports,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// Runtime is the container engine abstraction.
type Runtime interface {
	// Ping verifies the engine is reachable and returns its version.
	Ping(ctx context.Context) (version string, err error)
	// Pull fetches an image by reference (optionally digest-pinned).
	Pull(ctx context.Context, spec *PullSpec) error
	// PrepareHost applies the bounded host-memory controls on a managed spec.
	PrepareHost(ctx context.Context, spec *ContainerSpec) error
	// Create materializes a stopped container from the spec.
	Create(ctx context.Context, spec *ContainerSpec) (string, error)
	// Start launches a created container.
	Start(ctx context.Context, id string) error
	// Stop halts a running container within the given timeout (0 = engine default).
	Stop(ctx context.Context, id string, timeoutSeconds int) error
	// Remove deletes a container (force removes running).
	Remove(ctx context.Context, id string, force bool) error
	// Inspect returns observed state for one container.
	Inspect(ctx context.Context, idOrName string) (*ContainerInfo, error)
	// ListByLabel lists containers carrying the label with the given value.
	ListByLabel(ctx context.Context, key, value string) ([]ContainerInfo, error)
	// LogsFollow streams container output (interleaved); the caller owns
	// closing rc.
	LogsFollow(ctx context.Context, id string, fromStdout, fromStderr bool) (io.ReadCloser, error)
	// LogsStreams streams container output with stdout and stderr
	// separated; the caller owns closing both.
	LogsStreams(ctx context.Context, id string) (stdout, stderr io.ReadCloser, err error)
}

// PullSpec identifies an image pull.
type PullSpec struct {
	Reference string `json:"reference"`
	Auth      *Auth  `json:"auth,omitempty"`
}

// Auth is registry credential material.
type Auth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Label keys for LMW-managed containers. Only containers carrying the
// managed label are ever touched by the runtime.
const (
	LabelManaged       = "dev.localmodelworks.managed"
	LabelDeployment    = "dev.localmodelworks.deployment"
	LabelRun           = "dev.localmodelworks.run"
	LabelRecipe        = "dev.localmodelworks.recipe"
	LabelRecipeVersion = "dev.localmodelworks.recipe-version"
	LabelRank          = "dev.localmodelworks.rank"
	LabelModule        = "dev.localmodelworks.module"
)

const HostPreparationModule = "host-preparation"

// ManagedLabels builds the label set for a workload.
func ManagedLabels(deployment, run, recipeDigest, recipeVersion string, rank int, module string) map[string]string {
	l := map[string]string{
		LabelManaged: "true",
		LabelRecipe:  recipeDigest,
	}
	if deployment != "" {
		l[LabelDeployment] = deployment
	}
	if run != "" {
		l[LabelRun] = run
	}
	if recipeVersion != "" {
		l[LabelRecipeVersion] = recipeVersion
	}
	if module != "" {
		l[LabelModule] = module
	}
	if rank >= 0 {
		l[LabelRank] = itoa(rank)
	}
	return l
}

// ValidateManagedSpec rejects any create request that cannot be tied to one
// LMW deployment or run. The runtime never creates an unowned container.
func ValidateManagedSpec(spec *ContainerSpec) error {
	if spec == nil || spec.Labels == nil || spec.Labels[LabelManaged] != "true" {
		return fmt.Errorf("container.unmanaged")
	}
	if spec.Labels[LabelDeployment] == "" && spec.Labels[LabelRun] == "" {
		return fmt.Errorf("container.identity_missing")
	}
	rank, err := strconv.Atoi(spec.Labels[LabelRank])
	if err != nil || rank < 0 {
		return fmt.Errorf("container.rank_invalid")
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
