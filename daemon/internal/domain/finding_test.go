package domain_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestClassificationHasNoFixedVerdict is acceptance criterion 7 (annotation
// half): the classification carries a version and has no field by which a
// classifier could declare a finding fixed. The classifier cannot mark a
// finding fixed (plan §5.12), so no such field may exist.
func TestClassificationHasNoFixedVerdict(t *testing.T) {
	rt := reflect.TypeOf(domain.Classification{})
	var hasVersion bool
	for i := range rt.NumField() {
		name := strings.ToLower(rt.Field(i).Name)
		if name == "version" {
			hasVersion = true
		}
		if strings.Contains(name, "fixed") || strings.Contains(name, "verdict") || name == "resolved" {
			t.Errorf("Classification exposes %q; the classifier can never declare a finding fixed", rt.Field(i).Name)
		}
	}
	if !hasVersion {
		t.Error("Classification has no Version field; it must be a versioned annotation")
	}
}

// TestClassificationValidate checks the finding join key and the positive
// version counter.
func TestClassificationValidate(t *testing.T) {
	if err := (domain.Classification{FindingID: "f1", Version: 1}).Validate(); err != nil {
		t.Fatalf("valid classification rejected: %v", err)
	}
	if err := (domain.Classification{Version: 1}).Validate(); !errors.Is(err, domain.ErrEmptyID) {
		t.Errorf("classification without finding_id accepted")
	}
	if err := (domain.Classification{FindingID: "f1", Version: 0}).Validate(); !errors.Is(err, domain.ErrNonPositive) {
		t.Errorf("classification with non-positive version accepted")
	}
}

// TestClassificationAnnotateIsNewVersion checks a correction is a new version,
// not an in-place edit: Annotate increments the version and returns a new value,
// leaving the receiver unchanged.
func TestClassificationAnnotateIsNewVersion(t *testing.T) {
	c := domain.Classification{FindingID: "f1", Version: 1, Materiality: "low", Note: "first pass"}
	next := c.Annotate("high", "confident", "on reflection, material")
	if next.Version != 2 {
		t.Errorf("annotated version = %d, want 2", next.Version)
	}
	if c.Version != 1 || c.Materiality != "low" {
		t.Error("Annotate mutated the receiver; corrections must be new versions")
	}
	if next.Materiality != "high" {
		t.Errorf("annotated materiality = %q, want high", next.Materiality)
	}
}

// TestFindingHasNoMutators is acceptance criterion 7 (immutability half): a raw
// Finding is immutable. It exposes no pointer-receiver methods (the only way a
// method could mutate a struct value) and no verdict field of its own.
func TestFindingHasNoMutators(t *testing.T) {
	pt := reflect.TypeOf(&domain.Finding{})
	for i := range pt.NumMethod() {
		// Validate is a value-receiver method and appears on the value type
		// too; a pointer-only method would be a mutator.
		m := pt.Method(i)
		if _, onValue := reflect.TypeOf(domain.Finding{}).MethodByName(m.Name); !onValue {
			t.Errorf("Finding has pointer-only method %q; a raw finding must be immutable", m.Name)
		}
	}
	rt := reflect.TypeOf(domain.Finding{})
	for i := range rt.NumField() {
		if name := strings.ToLower(rt.Field(i).Name); strings.Contains(name, "fixed") || strings.Contains(name, "verdict") {
			t.Errorf("Finding exposes %q; a raw finding carries no verdict", rt.Field(i).Name)
		}
	}
}

func TestFindingValidate(t *testing.T) {
	f := domain.Finding{ID: "f1", RunID: "run-1", Message: "x", CreatedAt: time.Now()}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid finding rejected: %v", err)
	}
	if err := (domain.Finding{RunID: "run-1"}).Validate(); err == nil {
		t.Error("finding without id accepted")
	}
	if err := (domain.Finding{ID: "f1", RunID: "run-1"}).Validate(); err == nil {
		t.Error("finding without created_at accepted")
	}
}
