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
// extra CPU, and once a build has touched guest memory it costs the host 137
// to 155 MiB more than the declared size until it is deleted. The budget
// covers that with a per-VM overhead (VMOverheadMiB; ward density spike,
// devlog/2026-11-02-1210-vm-overhead-reservation.md).
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
// default, fails: policy may raise a project's size, never lower it. Under a
// budget, a size whose reservation can never fit fails too, naming the size,
// the overhead, and the budget; a bound with no budget checks nothing.
func ResolveLaunchSizes(keys []domain.PolicyKey, bound LaunchBound) (LaunchSizes, error) {
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
	for _, size := range []ContainerSize{writer, verification} {
		if _, err := bound.Reservation(size); err != nil {
			return LaunchSizes{}, fmt.Errorf("%w: %w", ErrLaunchSizePolicy, err)
		}
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

// RuntimeNone is the runtime version of a composition that starts no
// container VM (disabled, fake, scenario). Its launches cost the host nothing
// beyond their declared size, so its overhead is zero; a budget given to such
// a composition still reserves and holds like a real one.
const RuntimeNone = "none"

// vmOverheadMiB is the per-VM overhead recorded per container runtime version
// (plan §5.4, revision 105). It is a fixed value, never measured at
// conformance time: the overhead appears only once a build touches guest
// memory. Apple container 1.1.0 measured 137 to 155 MiB; 256 leaves margin for
// images and sizes the measurement didn't build. A new runtime version records
// its own value here before it runs launches under a budget.
var vmOverheadMiB = map[string]int{
	RuntimeNone:             0,
	"apple-container 1.1.0": 256,
}

// VMOverheadMiB returns the recorded per-VM overhead of a runtime version, and
// false when none is recorded.
func VMOverheadMiB(runtimeVersion string) (int, bool) {
	overhead, ok := vmOverheadMiB[runtimeVersion]
	return overhead, ok
}

// LaunchBound is what one daemon instance bounds its launches by: its memory
// budget and the container runtime version whose overhead every reservation
// adds. A zero BudgetMiB means no budget, and nothing is reserved.
type LaunchBound struct {
	BudgetMiB      int
	RuntimeVersion string
}

// Budgeted reports whether the bound reserves anything.
func (b LaunchBound) Budgeted() bool { return b.BudgetMiB > 0 }

// Reservation returns what a launch of size reserves under the bound: its
// declared memory plus the runtime's per-VM overhead. With no budget it
// reserves nothing and returns 0. It refuses a runtime version with no
// recorded overhead (domain.RuntimeOverheadUnknownError) and a reservation
// larger than the whole budget (domain.LaunchNeverFitsError).
func (b LaunchBound) Reservation(size ContainerSize) (int, error) {
	if !b.Budgeted() {
		return 0, nil
	}
	overhead, ok := VMOverheadMiB(b.RuntimeVersion)
	if !ok {
		return 0, &domain.RuntimeOverheadUnknownError{RuntimeVersion: b.RuntimeVersion}
	}
	if size.MemoryMiB+overhead > b.BudgetMiB {
		return 0, &domain.LaunchNeverFitsError{
			SizeMiB: size.MemoryMiB, OverheadMiB: overhead, BudgetMiB: b.BudgetMiB,
		}
	}
	return size.MemoryMiB + overhead, nil
}

// LaunchReservation builds the reservation record a launch of size makes
// under the bound. ok is false with no budget, when nothing is reserved.
func (b LaunchBound) LaunchReservation(
	invocation domain.InvocationID, run domain.RunID, kind domain.LaunchReservationKind, size ContainerSize,
) (domain.LaunchReservation, bool, error) {
	total, err := b.Reservation(size)
	if err != nil || total == 0 {
		return domain.LaunchReservation{}, false, err
	}
	return domain.LaunchReservation{
		InvocationID: invocation, RunID: run, Kind: kind,
		MemoryMiB: size.MemoryMiB, OverheadMiB: total - size.MemoryMiB,
	}, true, nil
}
