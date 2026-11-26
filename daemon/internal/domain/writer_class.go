package domain

import (
	"fmt"
	"time"
)

// ValidateWriterClass refuses a class a writer may not run on. Only the two
// writer classes qualify; the host verification class and any unregistered
// string do not.
func ValidateWriterClass(class RunnerBackendClass) error {
	switch class {
	case BackendCordonedHostProcess, BackendFreshVMReadOnlyVolumeHandoff:
		return nil
	case BackendCredentialFreeHostVerification:
	}
	return fmt.Errorf("writer class %q: %w", class, ErrInvalidWriterClass)
}

// TaskWriterClass is one task's requirement that its writers run warded.
// It only ever asks for the warded class: a task can't move itself onto the
// cordon when its project is warded, so the record holds no other class.
type TaskWriterClass struct {
	TaskID TaskID             `json:"task_id"`
	Class  RunnerBackendClass `json:"class"`
	// SetBy is who set it: cli:<username> for an operator command.
	SetBy string    `json:"set_by"`
	SetAt time.Time `json:"set_at"`
}

// Validate refuses a requirement for any class but the warded one, and one
// that lacks its task, setter, or UTC time. The store re-runs it on every
// read, so a stored row naming another class fails the read.
func (r TaskWriterClass) Validate() error {
	if r.TaskID == "" {
		return fmt.Errorf("task writer class task id: %w", ErrEmptyID)
	}
	if r.Class != BackendFreshVMReadOnlyVolumeHandoff {
		return fmt.Errorf("task %q writer class %q: %w", r.TaskID, r.Class, ErrTaskWriterClassNotWarded)
	}
	if err := validateSettingsSetter(r.SetBy, r.SetAt); err != nil {
		return fmt.Errorf("task %q writer class %w", r.TaskID, err)
	}
	return nil
}

// ResolveWriterClass returns the class a task's writer runs on: warded
// (BackendFreshVMReadOnlyVolumeHandoff) when the project names warded or the
// task requires it, and otherwise the default, cordoned
// (BackendCordonedHostProcess; plan §5.7, Cordoned Runs).
// project is the project settings' writer_class, nil when unset; task is the
// task's requirement, nil when it has none; hasImage is whether the project
// has a project image. A warded result for a project with no image is
// refused (ErrWardedWithoutProjectImage), since the warded writer runs in
// that image: the settings and the task requirement are each refused on
// write, but a recipe binding set after a task's requirement reaches here.
//
// The resolver decides which containment a writer runs under, so its caller
// (#3472's writer admission) is where it applies: that caller scopes it to
// writer roles and owns what a change after a task's first writer admission
// does. A project value the settings gate would refuse can't reach here,
// because the run settings loader re-checks it before a run composes; the
// resolver still refuses one rather than default it to the cordon.
func ResolveWriterClass(project *RunnerBackendClass, task *TaskWriterClass, hasImage bool) (RunnerBackendClass, error) {
	class, err := resolveWriterClass(project, task)
	if err != nil {
		return "", err
	}
	if class == BackendFreshVMReadOnlyVolumeHandoff && !hasImage {
		return "", fmt.Errorf("writer class %q: %w", class, ErrWardedWithoutProjectImage)
	}
	return class, nil
}

func resolveWriterClass(project *RunnerBackendClass, task *TaskWriterClass) (RunnerBackendClass, error) {
	if task != nil {
		if err := task.Validate(); err != nil {
			return "", err
		}
		return BackendFreshVMReadOnlyVolumeHandoff, nil
	}
	if project == nil {
		return BackendCordonedHostProcess, nil
	}
	if err := ValidateWriterClass(*project); err != nil {
		return "", err
	}
	return *project, nil
}
