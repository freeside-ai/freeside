package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func wardedTask() *domain.TaskWriterClass {
	return &domain.TaskWriterClass{
		TaskID: "task-1", Class: domain.BackendFreshVMReadOnlyVolumeHandoff,
		SetBy: "cli:operator", SetAt: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC),
	}
}

func TestResolveWriterClass(t *testing.T) {
	cordoned, warded := domain.BackendCordonedHostProcess, domain.BackendFreshVMReadOnlyVolumeHandoff
	for _, tc := range []struct {
		name    string
		project *domain.RunnerBackendClass
		task    *domain.TaskWriterClass
		want    domain.RunnerBackendClass
	}{
		{"neither setting", nil, nil, domain.BackendCordonedHostProcess},
		{"project names cordoned", &cordoned, nil, domain.BackendCordonedHostProcess},
		{"project names warded", &warded, nil, domain.BackendFreshVMReadOnlyVolumeHandoff},
		{"task requires warded under an unset project", nil, wardedTask(), domain.BackendFreshVMReadOnlyVolumeHandoff},
		{"task requires warded under a cordoned project", &cordoned, wardedTask(), domain.BackendFreshVMReadOnlyVolumeHandoff},
	} {
		got, err := domain.ResolveWriterClass(tc.project, tc.task, true)
		if err != nil || got != tc.want {
			t.Errorf("%s: ResolveWriterClass = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// A project with no image resolves to the cordon by default, and a warded
// project setting or task requirement is refused, naming the missing image.
func TestResolveWriterClassRefusesWardedWithoutProjectImage(t *testing.T) {
	cordoned, warded := domain.BackendCordonedHostProcess, domain.BackendFreshVMReadOnlyVolumeHandoff
	if got, err := domain.ResolveWriterClass(&cordoned, nil, false); err != nil || got != cordoned {
		t.Errorf("cordoned project with no image: ResolveWriterClass = %q, %v; want %q", got, err, cordoned)
	}
	for name, tc := range map[string]struct {
		project *domain.RunnerBackendClass
		task    *domain.TaskWriterClass
	}{
		"project names warded": {&warded, nil},
		"task requires warded": {nil, wardedTask()},
	} {
		_, err := domain.ResolveWriterClass(tc.project, tc.task, false)
		if !errors.Is(err, domain.ErrWardedWithoutProjectImage) {
			t.Errorf("%s: err = %v, want ErrWardedWithoutProjectImage", name, err)
		}
	}
}

func TestResolveWriterClassRefusesWhatTheGatesRefuse(t *testing.T) {
	verification := domain.BackendCredentialFreeHostVerification
	if _, err := domain.ResolveWriterClass(&verification, nil, true); !errors.Is(err, domain.ErrInvalidWriterClass) {
		t.Errorf("project naming the verification class: err = %v, want ErrInvalidWriterClass", err)
	}
	task := wardedTask()
	task.Class = domain.BackendCordonedHostProcess
	if _, err := domain.ResolveWriterClass(nil, task, true); !errors.Is(err, domain.ErrTaskWriterClassNotWarded) {
		t.Errorf("task naming the cordon: err = %v, want ErrTaskWriterClassNotWarded", err)
	}
}

func TestValidateWriterClass(t *testing.T) {
	for _, class := range domain.AllRunnerBackendClasses {
		err := domain.ValidateWriterClass(class)
		writer := class == domain.BackendCordonedHostProcess || class == domain.BackendFreshVMReadOnlyVolumeHandoff
		if writer != (err == nil) {
			t.Errorf("ValidateWriterClass(%q) = %v, want writer class %t", class, err, writer)
		}
	}
	if err := domain.ValidateWriterClass("warded"); !errors.Is(err, domain.ErrInvalidWriterClass) {
		t.Errorf("unknown string: err = %v, want ErrInvalidWriterClass", err)
	}
}

func TestTaskWriterClassValidate(t *testing.T) {
	if err := wardedTask().Validate(); err != nil {
		t.Fatalf("warded requirement: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*domain.TaskWriterClass)
	}{
		{"no task", func(r *domain.TaskWriterClass) { r.TaskID = "" }},
		{"cordoned class", func(r *domain.TaskWriterClass) { r.Class = domain.BackendCordonedHostProcess }},
		{"unknown class", func(r *domain.TaskWriterClass) { r.Class = "warded" }},
		{"no setter", func(r *domain.TaskWriterClass) { r.SetBy = "" }},
		{"non-UTC time", func(r *domain.TaskWriterClass) { r.SetAt = r.SetAt.In(time.FixedZone("x", 3600)) }},
	} {
		r := wardedTask()
		tc.mutate(r)
		if r.Validate() == nil {
			t.Errorf("%s: Validate accepted %+v", tc.name, *r)
		}
	}
}
