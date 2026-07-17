package ward

import "context"

// MountType is the kind of a container mount as the runtime reports it. Only
// named volumes are ever allowed by the gate; every other kind (a host bind,
// or any kind this vocabulary does not know) fails verification, so an
// unknown runtime mount kind fails closed. The zero value "" is invalid by
// design.
type MountType string

const (
	// MountVolume is a named volume managed by the container runtime.
	MountVolume MountType = "volume"
	// MountBind is a host-directory bind (virtiofs on Apple container). The
	// gate never generates one and rejects any it observes.
	MountBind MountType = "bind"
)

// AllMountTypes lists every valid MountType; it drives table-driven tests
// and is the single place a new mount type is registered.
var AllMountTypes = []MountType{MountVolume, MountBind}

func (m MountType) valid() bool {
	switch m {
	case MountVolume, MountBind:
		return true
	default:
		return false
	}
}

// ContainerState is a container's observed lifecycle state. The gate treats
// exactly StateStopped as proof the VM is gone (spike: a stopped writer
// releases its volume attachment); any other value, known or not, is "not
// stopped", so an unknown state fails closed into a timeout. The zero value
// "" is invalid by design.
type ContainerState string

const (
	// StateRunning: the container's VM is live.
	StateRunning ContainerState = "running"
	// StateStopped: no VM holds the container's attachments. Apple container
	// reports created-but-never-started containers as stopped too; both mean
	// no live VM.
	StateStopped ContainerState = "stopped"
)

// AllContainerStates lists every valid ContainerState; it drives
// table-driven tests and is the single place a new state is registered.
var AllContainerStates = []ContainerState{StateRunning, StateStopped}

func (s ContainerState) valid() bool {
	switch s {
	case StateRunning, StateStopped:
		return true
	default:
		return false
	}
}

// Mount is one container mount, in both the specs the gate generates and the
// reports the runtime returns. Source is the volume name for MountVolume and
// the host path for MountBind.
type Mount struct {
	Type     MountType `json:"type"`
	Source   string    `json:"source"`
	Target   string    `json:"target"`
	ReadOnly bool      `json:"read_only"`
}

// Label is one metadata label on a container or volume. A slice of labels,
// sorted by key, stands in for a map so generated specs stay deterministic
// and golden-pinnable.
type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ContainerSpec is the gate-generated creation request for a container. It
// deliberately cannot express SSH forwarding, published sockets or ports, or
// host binds beyond Mounts: what the vocabulary cannot say, the runtime is
// never asked for (checks 2 and 4 then verify the runtime didn't add any).
type ContainerSpec struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command"`
	Env     []string `json:"env"`
	Mounts  []Mount  `json:"mounts"`
	Labels  []Label  `json:"labels"`
}

// InspectReport is the runtime's observed configuration and state for one
// container; check 3 reads State and check 4 verifies the rest against the
// generated allowlist.
type InspectReport struct {
	State ContainerState
	// Mounts are the persistent mounts the runtime will realize. An
	// implementation maps unknown mount kinds to an invalid MountType rather
	// than dropping them, so verification sees and rejects them.
	Mounts []Mount
	// Env is the container's full process environment.
	Env []string
	// SSH reports whether SSH agent forwarding is configured.
	SSH bool
	// PublishedSockets and PublishedPorts list configured host publications.
	PublishedSockets []string
	PublishedPorts   []string
}

// VolumeSummary identifies one named volume and its labels.
type VolumeSummary struct {
	Name   string
	Labels []Label
	// LabelsObserved distinguishes an explicitly empty label set from an
	// omitted runtime field that cannot prove or disprove ownership.
	LabelsObserved bool
}

// ContainerSummary identifies one container in a full listing.
type ContainerSummary struct {
	ID     string
	State  ContainerState
	Labels []Label
	// LabelsObserved has the same trust-boundary meaning as on VolumeSummary.
	LabelsObserved bool
}

// Runtime is the seam between the gate and the container runtime. The real
// implementation (CLIRuntime) shells out to Apple container; tests script a
// fake. The gate trusts nothing a Runtime returns: every security-relevant
// answer is re-verified against the generated allowlist or the observed
// state, and a Runtime error always fails the gate closed.
type Runtime interface {
	// CreateVolume creates the named volume with the given size and labels.
	CreateVolume(ctx context.Context, name string, sizeMB int64, labels []Label) error
	// DeleteVolume deletes the named volume.
	DeleteVolume(ctx context.Context, name string) error
	// ListVolumes returns every volume the runtime knows.
	ListVolumes(ctx context.Context) ([]VolumeSummary, error)
	// CreateContainer creates (without starting) a container from spec.
	CreateContainer(ctx context.Context, spec ContainerSpec) error
	// StartContainer starts a created container.
	StartContainer(ctx context.Context, id string) error
	// StopContainer stops a running container. The gate never uses it on the
	// happy path (the writer must be observed to stop on its own); teardown
	// uses it to reap a hung container after the gate already failed.
	StopContainer(ctx context.Context, id string) error
	// Inspect returns the observed configuration and state of a container.
	Inspect(ctx context.Context, id string) (InspectReport, error)
	// DeleteContainer deletes a stopped container.
	DeleteContainer(ctx context.Context, id string) error
	// ListContainers returns every container the runtime knows, including
	// stopped ones (container list --all).
	ListContainers(ctx context.Context) ([]ContainerSummary, error)
	// ExportRootFS writes the stopped container's root filesystem as a tar
	// archive to destPath on the host.
	ExportRootFS(ctx context.Context, id, destPath string) error
}
