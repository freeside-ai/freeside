package cordon

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The declaration gate's refusals. The suite's own tests (suite_test.go)
// drive it end to end; these pin the generation guard itself.

func declares(b *Backend) bool {
	return slices.Equal(b.Capabilities().Snapshot(), domain.NewCapabilitySnapshot(domain.CordonedCapabilities()...))
}

func TestANewBackendDeclaresNothing(t *testing.T) {
	t.Parallel()
	b, err := New(testConfig(t), &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Capabilities()) != 0 {
		t.Fatalf("a backend no pass has proved declares %v", b.Capabilities().Snapshot())
	}
}

func TestDeclarationNeedsACurrentRecordedPass(t *testing.T) {
	t.Parallel()
	recordErr := errors.New("disk full")
	cases := map[string]struct {
		proved   bool
		record   func() error
		stale    bool
		declares bool
	}{
		"passed":            {proved: true, declares: true},
		"failed":            {proved: false},
		"record failed":     {proved: true, record: func() error { return recordErr }},
		"stale generation":  {proved: true, stale: true},
		"passed, no record": {proved: true, record: func() error { return nil }, declares: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, err := New(testConfig(t), &fakeSeeder{})
			if err != nil {
				t.Fatal(err)
			}
			generation, err := b.beginConformanceProof(nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.stale {
				if _, err := b.beginConformanceProof(nil); err != nil {
					t.Fatal(err)
				}
			}
			published, err := b.concludeConformanceProof(generation, c.proved, c.record)
			if published == c.stale {
				t.Fatalf("published = %v for a stale = %v generation", published, c.stale)
			}
			if wantErr := name == "record failed"; errors.Is(err, recordErr) != wantErr {
				t.Fatalf("conclude = %v", err)
			}
			if got := declares(b); got != c.declares {
				t.Fatalf("declares = %v, want %v", got, c.declares)
			}
		})
	}
}

func TestBeginningAPassWithdrawsTheDeclaration(t *testing.T) {
	t.Parallel()
	b, err := New(testConfig(t), &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := b.beginConformanceProof(nil)
	if _, err := b.concludeConformanceProof(generation, true, nil); err != nil || !declares(b) {
		t.Fatalf("a passed proof did not declare: %v", err)
	}
	markerErr := errors.New("marker not recorded")
	if _, err := b.beginConformanceProof(func() error { return markerErr }); !errors.Is(err, markerErr) {
		t.Fatalf("begin = %v, want the marker's error", err)
	}
	if declares(b) {
		t.Fatal("a begun pass whose marker failed left the declaration standing")
	}
}

func TestWithdrawalSupersedesAPassInFlight(t *testing.T) {
	t.Parallel()
	b, err := New(testConfig(t), &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := b.beginConformanceProof(nil)
	if _, err := b.concludeConformanceProof(first, true, nil); err != nil {
		t.Fatal(err)
	}
	inFlight, _ := b.beginConformanceProof(nil)
	recorded := false
	if err := b.withdrawConformance(func() error { recorded = true; return nil }); err != nil || !recorded {
		t.Fatalf("withdraw = %v, recorded %v", err, recorded)
	}
	if declares(b) {
		t.Fatal("a withdrawn backend still declares")
	}
	if published, _ := b.concludeConformanceProof(inFlight, true, nil); published || declares(b) {
		t.Fatal("a pass that began before the withdrawal published over it")
	}
}
