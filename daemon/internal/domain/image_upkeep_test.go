package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validUpkeepRefusalInput() domain.ImageUpkeepRefusalInput {
	return domain.ImageUpkeepRefusalInput{
		RepositoryID: 1278475858, Repository: "freeasinbird/gh-imgup",
		Harness: domain.HarnessClientClaudeCode, TipSHA: strings.Repeat("e", 40),
		MeasuredFromImageID: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		Class:               domain.ImageUpkeepRefusalRepair,
		Findings: []domain.ImageUpkeepClauseFinding{{
			Clause: domain.RebuildRefusalLockfileInconsistent, Detail: "lockfile root disagrees",
		}},
		RecordedAt: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
}

// The exported preimage is what #2336 stores as the maintenance task's source
// artifact: the blob store accepts it only because it hashes to the refusal's
// ID, and a nil and an empty Findings list must yield the same bytes because
// both decode to the same ID.
func TestImageUpkeepRefusalPreimageAddressesItsID(t *testing.T) {
	t.Parallel()
	refusal, err := domain.NewImageUpkeepRefusal(validUpkeepRefusalInput())
	if err != nil {
		t.Fatalf("valid refusal: %v", err)
	}
	preimage, err := refusal.Preimage()
	if err != nil {
		t.Fatalf("Preimage: %v", err)
	}
	if got := contentaddr.Sum(preimage); got != string(refusal.ID) {
		t.Fatalf("preimage addresses %s, want the refusal id %s", got, refusal.ID)
	}

	nilFindings, emptyFindings := refusal, refusal
	nilFindings.Findings = nil
	emptyFindings.Findings = []domain.ImageUpkeepClauseFinding{}
	nilBytes, err := nilFindings.Preimage()
	if err != nil {
		t.Fatalf("nil-findings Preimage: %v", err)
	}
	emptyBytes, err := emptyFindings.Preimage()
	if err != nil {
		t.Fatalf("empty-findings Preimage: %v", err)
	}
	if string(nilBytes) != string(emptyBytes) {
		t.Fatalf("nil and empty findings encode differently:\n nil=%s\n empty=%s", nilBytes, emptyBytes)
	}
}

func TestImageUpkeepRefusalValidate(t *testing.T) {
	t.Parallel()
	valid, err := domain.NewImageUpkeepRefusal(validUpkeepRefusalInput())
	if err != nil {
		t.Fatalf("valid refusal: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*domain.ImageUpkeepRefusalInput)
		want   error
	}{
		"repository id": {func(in *domain.ImageUpkeepRefusalInput) { in.RepositoryID = 0 }, domain.ErrNonPositive},
		"repository": {
			func(in *domain.ImageUpkeepRefusalInput) { in.Repository = "../x" },
			domain.ErrImageUpkeepRefusalInvalid,
		},
		"harness": {
			func(in *domain.ImageUpkeepRefusalInput) { in.Harness = "" },
			domain.ErrInvalidHarnessClientKind,
		},
		"tip sha": {
			func(in *domain.ImageUpkeepRefusalInput) { in.TipSHA = strings.Repeat("E", 40) },
			domain.ErrImageUpkeepRefusalInvalid,
		},
		"measured from": {
			func(in *domain.ImageUpkeepRefusalInput) { in.MeasuredFromImageID = "sha256:short" },
			domain.ErrImageUpkeepRefusalInvalid,
		},
		"class": {
			func(in *domain.ImageUpkeepRefusalInput) { in.Class = "" },
			domain.ErrInvalidImageUpkeepRefusalClass,
		},
		"no clause outside host fault": {
			func(in *domain.ImageUpkeepRefusalInput) { in.Findings = nil },
			domain.ErrImageUpkeepRefusalInvalid,
		},
		"unknown clause": {
			func(in *domain.ImageUpkeepRefusalInput) { in.Findings[0].Clause = "" },
			domain.ErrImageUpkeepRefusalInvalid,
		},
		"detail not utf-8": {func(in *domain.ImageUpkeepRefusalInput) {
			in.Findings[0].Detail = "bad \xff byte"
		}, domain.ErrImageUpkeepRefusalInvalid},
		"detail over bound": {func(in *domain.ImageUpkeepRefusalInput) {
			in.Findings[0].Detail = strings.Repeat("x", domain.MaxImageUpkeepDetailBytes+1)
		}, domain.ErrImageUpkeepRefusalInvalid},
		"recorded at zero": {
			func(in *domain.ImageUpkeepRefusalInput) { in.RecordedAt = time.Time{} },
			domain.ErrMissingTimestamp,
		},
		"recorded at local": {func(in *domain.ImageUpkeepRefusalInput) {
			in.RecordedAt = in.RecordedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
	} {
		in := validUpkeepRefusalInput()
		tc.mutate(&in)
		if _, err := domain.NewImageUpkeepRefusal(in); !errors.Is(err, tc.want) {
			t.Errorf("%s: NewImageUpkeepRefusal = %v, want %v", name, err, tc.want)
		}
	}

	hostFault := validUpkeepRefusalInput()
	hostFault.Class, hostFault.Findings = domain.ImageUpkeepRefusalHostFault, nil
	if _, err := domain.NewImageUpkeepRefusal(hostFault); err != nil {
		t.Errorf("host fault with no clause: %v", err)
	}

	// The ID addresses every fact but RecordedAt: a later replay of the same
	// refusal converges, and any altered fact no longer matches its ID.
	later := validUpkeepRefusalInput()
	later.RecordedAt = later.RecordedAt.Add(time.Hour)
	replayed, err := domain.NewImageUpkeepRefusal(later)
	if err != nil || replayed.ID != valid.ID {
		t.Errorf("replay at a later time: id %s, err %v; want %s", replayed.ID, err, valid.ID)
	}
	// Findings take one canonical order, so a verdict reported in another
	// order, or with a finding repeated, addresses the same record; a record
	// that holds them out of that order is malformed.
	two := validUpkeepRefusalInput()
	second := domain.ImageUpkeepClauseFinding{Clause: domain.RebuildRefusalRecipeChanged, Detail: "recipe digest moved"}
	two.Findings = append(two.Findings, second)
	reordered := validUpkeepRefusalInput()
	reordered.Findings = []domain.ImageUpkeepClauseFinding{second, two.Findings[0], second}
	first, err := domain.NewImageUpkeepRefusal(two)
	if err != nil {
		t.Fatal(err)
	}
	other, err := domain.NewImageUpkeepRefusal(reordered)
	if err != nil || other.ID != first.ID || len(other.Findings) != 2 {
		t.Errorf("reordered findings: id %s, %d findings, err %v; want %s with 2", other.ID, len(other.Findings), err, first.ID)
	}
	unordered := first
	unordered.Findings = []domain.ImageUpkeepClauseFinding{first.Findings[1], first.Findings[0]}
	if err := unordered.Validate(); !errors.Is(err, domain.ErrImageUpkeepRefusalInvalid) {
		t.Errorf("out-of-order findings: Validate = %v, want ErrImageUpkeepRefusalInvalid", err)
	}

	for name, mutate := range map[string]func(*domain.ImageUpkeepRefusal){
		"class": func(r *domain.ImageUpkeepRefusal) { r.Class = domain.ImageUpkeepRefusalPolicy },
		"tip":   func(r *domain.ImageUpkeepRefusal) { r.TipSHA = strings.Repeat("f", 40) },
		"detail": func(r *domain.ImageUpkeepRefusal) {
			r.Findings = []domain.ImageUpkeepClauseFinding{{Clause: r.Findings[0].Clause, Detail: "other"}}
		},
		"harness": func(r *domain.ImageUpkeepRefusal) { r.Harness = domain.HarnessClientCodexCLI },
		"measured": func(r *domain.ImageUpkeepRefusal) {
			r.MeasuredFromImageID = domain.Digest("sha256:" + strings.Repeat("cd", 32))
		},
	} {
		altered := valid
		mutate(&altered)
		if err := altered.Validate(); !errors.Is(err, domain.ErrImageUpkeepRefusalInconsistent) {
			t.Errorf("altered %s: Validate = %v, want ErrImageUpkeepRefusalInconsistent", name, err)
		}
	}
}

func TestRepositoryTipObservationValidate(t *testing.T) {
	t.Parallel()
	valid := domain.RepositoryTipObservation{
		RepositoryID: 1278475858, Repository: "freeasinbird/gh-imgup", Branch: "main",
		TipSHA:     strings.Repeat("e", 40),
		ObservedAt: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid observation with no upkeep tip: %v", err)
	}
	valid.UpkeepTipSHA = strings.Repeat("a", 40)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid observation with an upkeep tip: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*domain.RepositoryTipObservation)
		want   error
	}{
		"repository id": {func(o *domain.RepositoryTipObservation) { o.RepositoryID = -1 }, domain.ErrNonPositive},
		"repository": {
			func(o *domain.RepositoryTipObservation) { o.Repository = "gh-imgup" },
			domain.ErrRepositoryTipObservationInvalid,
		},
		"empty branch": {
			func(o *domain.RepositoryTipObservation) { o.Branch = "" },
			domain.ErrRepositoryTipObservationInvalid,
		},
		"branch with space": {
			func(o *domain.RepositoryTipObservation) { o.Branch = "ma in" },
			domain.ErrRepositoryTipObservationInvalid,
		},
		"branch not utf-8": {
			func(o *domain.RepositoryTipObservation) { o.Branch = "main\xff" },
			domain.ErrRepositoryTipObservationInvalid,
		},
		"long branch": {
			func(o *domain.RepositoryTipObservation) { o.Branch = strings.Repeat("b", 256) },
			domain.ErrRepositoryTipObservationInvalid,
		},
		"tip sha": {
			func(o *domain.RepositoryTipObservation) { o.TipSHA = "abc" },
			domain.ErrRepositoryTipObservationInvalid,
		},
		"upkeep tip sha": {
			func(o *domain.RepositoryTipObservation) { o.UpkeepTipSHA = "abc" },
			domain.ErrRepositoryTipObservationInvalid,
		},
		"observed at zero": {
			func(o *domain.RepositoryTipObservation) { o.ObservedAt = time.Time{} },
			domain.ErrMissingTimestamp,
		},
		"observed at local": {func(o *domain.RepositoryTipObservation) {
			o.ObservedAt = o.ObservedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
	} {
		observation := valid
		tc.mutate(&observation)
		if err := observation.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", name, err, tc.want)
		}
	}
}
