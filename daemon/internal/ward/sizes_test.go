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
	sizes, err := ResolveLaunchSizes([]domain.PolicyKey{{Key: "paths", Value: "daemon/**"}})
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
	})
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
			_, err := ResolveLaunchSizes([]domain.PolicyKey{key})
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
	})
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
		want := sizeArgs(spec.Size)
		at := slices.Index(args, "--cpus")
		if at < 0 || !slices.Equal(args[at:at+len(want)], want) {
			t.Fatalf("%s: args %q do not declare %q", class, args, want)
		}
		if end := slices.Index(args, "--"); at > end {
			t.Fatalf("%s: size flags follow the positional terminator: %q", class, args)
		}
	}
	if got := sizeArgs(ContainerSize{CPUs: 6, MemoryMiB: 3072}); !slices.Equal(got,
		[]string{"--cpus", "6", "--memory", "3072M"}) {
		t.Fatalf("sizeArgs = %q", got)
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
