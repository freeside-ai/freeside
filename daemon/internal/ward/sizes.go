package ward

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// LaunchClass names the kind of launch a ward container belongs to. Ward knows
// a launch's class only from the code path that starts it; the class chooses
// the declared size. A helper (seeder, observer, exporter) takes the class of
// the launch it serves, because it runs one at a time inside that launch. The
// zero value "" is invalid by design.
type LaunchClass string

const (
	// LaunchWriter is every Backend.Handoff writer launch (specify,
	// implement, remediate) with its helpers.
	LaunchWriter LaunchClass = "writer"
	// LaunchReview is a Codex or Claude review launch, shadow review included,
	// with its workspace seed, snapshot seed, and observers.
	LaunchReview LaunchClass = "review"
	// LaunchVerification is every ProjectImageRoom container: recipe read,
	// preparation, and recipe commands.
	LaunchVerification LaunchClass = "verification"
	// LaunchConformance is the conformance suite's probes, the pre-job check,
	// and the credential-manifest probe.
	LaunchConformance LaunchClass = "conformance"
)

// AllLaunchClasses lists every valid LaunchClass; it drives table-driven tests
// and is the single place a new class is registered.
var AllLaunchClasses = []LaunchClass{
	LaunchWriter, LaunchReview, LaunchVerification, LaunchConformance,
}

func (c LaunchClass) valid() bool {
	switch c {
	case LaunchWriter, LaunchReview, LaunchVerification, LaunchConformance:
		return true
	default:
		return false
	}
}

// ContainerSize is the CPU cap and memory limit ward declares for a container.
// Apple container 1.1.0 enforces both inside the guest with a cgroup
// (cpu.max and memory.max hold exactly these values); the VM itself gets one
// extra CPU and roughly 100 MiB of memory on top for the guest's own use.
// The zero value is invalid: a launch with no declared size is refused rather
// than sent with the runtime's default.
type ContainerSize struct {
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memory_mib"`
}

// ErrInvalidContainerSize marks a size that cannot be declared to the runtime.
var ErrInvalidContainerSize = errors.New("invalid container size")

// minContainerMemoryMiB is Apple container 1.1.0's floor: create refuses
// "minimum memory amount allowed is 200 MiB". Refusing here keeps a
// too-small size a spec error rather than an opaque runtime failure.
const minContainerMemoryMiB = 200

func (s ContainerSize) validate() error {
	if s.CPUs < 1 || s.MemoryMiB < minContainerMemoryMiB {
		return fmt.Errorf("%w: %d CPUs and %d MiB; need at least 1 CPU and %d MiB",
			ErrInvalidContainerSize, s.CPUs, s.MemoryMiB, minContainerMemoryMiB)
	}
	return nil
}

func (s ContainerSize) String() string {
	return fmt.Sprintf("%d CPUs, %d MiB", s.CPUs, s.MemoryMiB)
}

// DefaultLaunchSize is the ward default for a class. Every real run before
// sizes were declared ran under the runtime's default of 4 CPUs and 1024 MiB,
// so review and conformance keep that, and the writer and verification
// classes, which run builds and tests, get twice the memory as headroom.
// devlog/2026-09-29-1810-ward-container-limits.md records the evidence.
func DefaultLaunchSize(class LaunchClass) ContainerSize {
	switch class {
	case LaunchWriter, LaunchVerification:
		return ContainerSize{CPUs: 4, MemoryMiB: 2048}
	case LaunchReview, LaunchConformance:
		return ContainerSize{CPUs: 4, MemoryMiB: 1024}
	}
	return ContainerSize{}
}

// Resolved-policy keys that raise a project's writer and verification sizes.
// Review and conformance sizes are ward defaults only.
const (
	PolicyWriterCPUs            = "execution.writer_cpus"
	PolicyWriterMemoryMiB       = "execution.writer_memory_mib"
	PolicyVerificationCPUs      = "execution.verification_cpus"
	PolicyVerificationMemoryMiB = "execution.verification_memory_mib"
)

// ErrLaunchSizePolicy marks a resolved policy whose size keys do not resolve.
var ErrLaunchSizePolicy = errors.New("launch size policy does not resolve")

// LaunchSizes are the sizes one run's policy resolves for its policy-sized
// launch classes.
type LaunchSizes struct {
	Writer       ContainerSize
	Verification ContainerSize
}

// ResolveLaunchSizes reads the writer and verification size keys from a
// run's resolved policy keys. The caller authenticates the keys (a validated
// domain.ResolvedPolicy, or a body its content digest re-checked). A missing
// key keeps the class default. A malformed value, or one below the class
// default, fails: policy may raise a project's size, never lower it.
func ResolveLaunchSizes(keys []domain.PolicyKey) (LaunchSizes, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if _, dup := values[key.Key]; dup {
			return LaunchSizes{}, fmt.Errorf("%w: key %s appears more than once", ErrLaunchSizePolicy, key.Key)
		}
		values[key.Key] = key.Value
	}
	writer, err := resolveClassSize(values, LaunchWriter, PolicyWriterCPUs, PolicyWriterMemoryMiB)
	if err != nil {
		return LaunchSizes{}, err
	}
	verification, err := resolveClassSize(
		values, LaunchVerification, PolicyVerificationCPUs, PolicyVerificationMemoryMiB,
	)
	if err != nil {
		return LaunchSizes{}, err
	}
	return LaunchSizes{Writer: writer, Verification: verification}, nil
}

func resolveClassSize(
	values map[string]string, class LaunchClass, cpusKey, memoryKey string,
) (ContainerSize, error) {
	size := DefaultLaunchSize(class)
	for _, field := range []struct {
		key   string
		value *int
	}{{cpusKey, &size.CPUs}, {memoryKey, &size.MemoryMiB}} {
		raw, ok := values[field.key]
		if !ok {
			continue
		}
		floor := *field.value
		parsed, err := strconv.Atoi(raw)
		// strconv.Itoa round-trips only canonical decimal, so "+2" and "02"
		// are refused as malformed rather than read as 2.
		if err != nil || strconv.Itoa(parsed) != raw {
			return ContainerSize{}, fmt.Errorf("%w: %s value %q is not a decimal integer",
				ErrLaunchSizePolicy, field.key, raw)
		}
		if parsed < floor {
			return ContainerSize{}, fmt.Errorf("%w: %s value %d is below the %s default %d",
				ErrLaunchSizePolicy, field.key, parsed, class, floor)
		}
		*field.value = parsed
	}
	return size, nil
}
