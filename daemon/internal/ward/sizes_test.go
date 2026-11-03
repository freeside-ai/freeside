package ward

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// testContainerSize sizes the containers tests create directly; the size
// itself is incidental to those tests, but the runtime refuses none.
var testContainerSize = DefaultLaunchSize(LaunchConformance)

func TestLaunchClassesAreValidAndSized(t *testing.T) {
	t.Parallel()
	for _, class := range AllLaunchClasses {
		if !class.valid() {
			t.Errorf("%q is registered but invalid", class)
		}
		if err := DefaultLaunchSize(class).validate(); err != nil {
			t.Errorf("default size of %q: %v", class, err)
		}
	}
	if LaunchClass("").valid() {
		t.Error("the zero class is valid")
	}
	if DefaultLaunchSize("") != (ContainerSize{}) {
		t.Error("the zero class has a default size")
	}
}

func TestResolveLaunchSizesDefaultsWithoutSizeKeys(t *testing.T) {
	t.Parallel()
	sizes, err := ResolveLaunchSizes([]domain.PolicyKey{{Key: "paths", Value: "daemon/**"}}, LaunchBound{})
	if err != nil {
		t.Fatal(err)
	}
	want := LaunchSizes{
		Writer:       DefaultLaunchSize(LaunchWriter),
		Verification: DefaultLaunchSize(LaunchVerification),
	}
	if sizes != want {
		t.Fatalf("sizes = %+v, want %+v", sizes, want)
	}
}

func TestResolveLaunchSizesRaisesOnlyTheNamedClass(t *testing.T) {
	t.Parallel()
	sizes, err := ResolveLaunchSizes([]domain.PolicyKey{
		{Key: PolicyWriterCPUs, Value: "6"},
		{Key: PolicyWriterMemoryMiB, Value: "8192"},
	}, LaunchBound{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (ContainerSize{CPUs: 6, MemoryMiB: 8192}); sizes.Writer != want {
		t.Fatalf("writer = %s, want %s", sizes.Writer, want)
	}
	if sizes.Verification != DefaultLaunchSize(LaunchVerification) {
		t.Fatalf("verification = %s, want the default", sizes.Verification)
	}
}

func TestResolveLaunchSizesRefusesMalformedOrLoweredValues(t *testing.T) {
	t.Parallel()
	for name, key := range map[string]domain.PolicyKey{
		"below writer memory default":       {Key: PolicyWriterMemoryMiB, Value: "1024"},
		"below verification cpus default":   {Key: PolicyVerificationCPUs, Value: "2"},
		"zero":                              {Key: PolicyWriterCPUs, Value: "0"},
		"negative":                          {Key: PolicyWriterCPUs, Value: "-4"},
		"unit suffix":                       {Key: PolicyWriterMemoryMiB, Value: "4096M"},
		"sign":                              {Key: PolicyWriterCPUs, Value: "+8"},
		"leading zero":                      {Key: PolicyVerificationMemoryMiB, Value: "04096"},
		"fraction":                          {Key: PolicyVerificationCPUs, Value: "4.5"},
		"empty":                             {Key: PolicyWriterCPUs, Value: ""},
		"whitespace":                        {Key: PolicyWriterCPUs, Value: " 8"},
		"overflow":                          {Key: PolicyWriterMemoryMiB, Value: "99999999999999999999"},
		"verification memory below default": {Key: PolicyVerificationMemoryMiB, Value: "1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveLaunchSizes([]domain.PolicyKey{key}, LaunchBound{})
			if !errors.Is(err, ErrLaunchSizePolicy) {
				t.Fatalf("err = %v, want ErrLaunchSizePolicy", err)
			}
		})
	}
}

func TestResolveLaunchSizesRefusesARepeatedKey(t *testing.T) {
	t.Parallel()
	_, err := ResolveLaunchSizes([]domain.PolicyKey{
		{Key: PolicyWriterCPUs, Value: "8"},
		{Key: PolicyWriterCPUs, Value: "4"},
	}, LaunchBound{})
	if !errors.Is(err, ErrLaunchSizePolicy) {
		t.Fatalf("err = %v, want ErrLaunchSizePolicy", err)
	}
}

func TestCreateContainerArgsDeclareTheSize(t *testing.T) {
	t.Parallel()
	for _, class := range AllLaunchClasses {
		spec := ContainerSpec{
			Name: "freeside-size", Image: "example.test/img@sha256:" + strings.Repeat("a", 64),
			NetworkDisabled: true, Size: DefaultLaunchSize(class),
		}
		args, err := createContainerArgs(spec)
		if err != nil {
			t.Fatalf("%s: %v", class, err)
		}
		want := SizeArgs(spec.Size)
		at := slices.Index(args, "--cpus")
		if at < 0 || !slices.Equal(args[at:at+len(want)], want) {
			t.Fatalf("%s: args %q do not declare %q", class, args, want)
		}
		if end := slices.Index(args, "--"); at > end {
			t.Fatalf("%s: size flags follow the positional terminator: %q", class, args)
		}
	}
	if got := SizeArgs(ContainerSize{CPUs: 6, MemoryMiB: 3072}); !slices.Equal(got,
		[]string{"--cpus", "6", "--memory", "3072M"}) {
		t.Fatalf("SizeArgs = %q", got)
	}
}

func TestCreateContainerArgsRefuseAnUnsizedSpec(t *testing.T) {
	t.Parallel()
	for name, size := range map[string]ContainerSize{
		"zero":                    {},
		"no cpus":                 {MemoryMiB: 1024},
		"no memory":               {CPUs: 4},
		"negative":                {CPUs: -1, MemoryMiB: 1024},
		"below the runtime floor": {CPUs: 1, MemoryMiB: minContainerMemoryMiB - 1},
	} {
		spec := ContainerSpec{
			Name: "freeside-unsized", Image: "example.test/img@sha256:" + strings.Repeat("a", 64),
			NetworkDisabled: true, Size: size,
		}
		if _, err := createContainerArgs(spec); !errors.Is(err, ErrInvalidContainerSize) {
			t.Fatalf("%s: err = %v, want ErrInvalidContainerSize", name, err)
		}
	}
}

func TestDecodeInspectReportsRealizedResources(t *testing.T) {
	t.Parallel()
	withResources := cliContainer{Configuration: cliConfiguration{
		Resources: &cliResources{CPUs: new(4), MemoryInBytes: new(int64(2048 << 20))},
	}}
	rep := withResources.toReport()
	if !rep.ResourcesObserved || !realizedSize(rep, ContainerSize{CPUs: 4, MemoryMiB: 2048}) {
		t.Fatalf("report = %+v, want 4 CPUs and 2048 MiB observed", rep)
	}
	if realizedSize(rep, ContainerSize{CPUs: 4, MemoryMiB: 1024}) {
		t.Fatal("a different memory size matched")
	}
	omitted := cliContainer{Configuration: cliConfiguration{
		Resources: &cliResources{CPUs: new(4)},
	}}
	if rep := omitted.toReport(); rep.ResourcesObserved || realizedSize(rep, ContainerSize{}) {
		t.Fatalf("an omitted memory field was observed: %+v", rep)
	}
}

// appleBound is a budget on the reference runtime, whose overhead is 256 MiB.
func appleBound(budget int) LaunchBound {
	return LaunchBound{BudgetMiB: budget, RuntimeVersion: "apple-container 1.1.0"}
}

func TestLaunchBoundReservesSizePlusOverhead(t *testing.T) {
	t.Parallel()
	size := ContainerSize{CPUs: 4, MemoryMiB: 2048}
	if got, err := appleBound(8192).Reservation(size); err != nil || got != 2048+256 {
		t.Fatalf("reservation = %d, %v; want %d", got, err, 2048+256)
	}
	if got, err := appleBound(2048 + 256).Reservation(size); err != nil || got != 2048+256 {
		t.Fatalf("reservation at the exact budget = %d, %v", got, err)
	}
	if got, err := (LaunchBound{}).Reservation(size); err != nil || got != 0 {
		t.Fatalf("no budget reserves %d, %v; want 0", got, err)
	}
	if got, err := (LaunchBound{BudgetMiB: 4096, RuntimeVersion: RuntimeNone}).Reservation(size); err != nil || got != 2048 {
		t.Fatalf("a VM-less composition reserves %d, %v; want the declared size alone", got, err)
	}
}

func TestLaunchBoundRefusesANeverFittingSizeNamingAllThreeNumbers(t *testing.T) {
	t.Parallel()
	_, err := appleBound(2048).Reservation(ContainerSize{CPUs: 4, MemoryMiB: 2048})
	var never *domain.LaunchNeverFitsError
	if !errors.As(err, &never) || *never != (domain.LaunchNeverFitsError{SizeMiB: 2048, OverheadMiB: 256, BudgetMiB: 2048}) {
		t.Fatalf("err = %v, want a never-fits refusal naming 2048, 256, and 2048", err)
	}
	for _, number := range []string{"2048 MiB declared", "256 MiB per-VM overhead", "2048 MiB budget"} {
		if !strings.Contains(err.Error(), number) {
			t.Errorf("%q does not name %q", err, number)
		}
	}
}

func TestLaunchBoundRefusesAnUnrecordedRuntimeVersion(t *testing.T) {
	t.Parallel()
	bound := LaunchBound{BudgetMiB: 8192, RuntimeVersion: "apple-container 1.2.0"}
	_, err := bound.Reservation(DefaultLaunchSize(LaunchReview))
	var unknown *domain.RuntimeOverheadUnknownError
	if !errors.As(err, &unknown) || unknown.RuntimeVersion != "apple-container 1.2.0" {
		t.Fatalf("err = %v, want the unrecorded version named", err)
	}
	if _, err := (LaunchBound{RuntimeVersion: "apple-container 1.2.0"}).Reservation(testContainerSize); err != nil {
		t.Fatalf("no budget refused an unrecorded runtime: %v", err)
	}
}

func TestResolveLaunchSizesRefusesPolicyThatCanNeverFitTheBudget(t *testing.T) {
	t.Parallel()
	keys := []domain.PolicyKey{{Key: PolicyVerificationMemoryMiB, Value: "8192"}}
	if _, err := ResolveLaunchSizes(keys, appleBound(8192+256)); err != nil {
		t.Fatalf("a reservation equal to the budget is refused: %v", err)
	}
	_, err := ResolveLaunchSizes(keys, appleBound(8192))
	if !errors.Is(err, ErrLaunchSizePolicy) || !errors.Is(err, domain.ErrLaunchNeverFits) {
		t.Fatalf("err = %v, want a policy refusal for a size that never fits", err)
	}
	if !strings.Contains(err.Error(), "8192 MiB declared plus 256 MiB per-VM overhead exceeds the 8192 MiB budget") {
		t.Fatalf("%q does not name the size, overhead, and budget", err)
	}
}

func TestParseRuntimeVersion(t *testing.T) {
	t.Parallel()
	got, err := ParseRuntimeVersion([]byte("container CLI version 1.1.0 (build: release, commit: 5973b9c)\n"))
	if err != nil || got != "apple-container 1.1.0" {
		t.Fatalf("version = %q, %v", got, err)
	}
	if _, ok := VMOverheadMiB(got); !ok {
		t.Fatalf("the reference runtime %q has no recorded overhead", got)
	}
	for _, out := range []string{"", "container CLI version", "1.1.0", "podman version 5.0.0"} {
		if v, err := ParseRuntimeVersion([]byte(out)); err == nil {
			t.Errorf("%q parsed as %q", out, v)
		}
	}
}
