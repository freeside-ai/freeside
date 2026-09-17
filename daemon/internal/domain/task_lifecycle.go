package domain

// TaskLifecycle is a display projection, independent of run lifecycle and WIP.
type TaskLifecycle string

const (
	TaskActive    TaskLifecycle = "active"
	TaskFinished  TaskLifecycle = "finished"
	TaskStopped   TaskLifecycle = "stopped"
	TaskAbandoned TaskLifecycle = "abandoned"
)

var AllTaskLifecycles = []TaskLifecycle{TaskActive, TaskFinished, TaskStopped, TaskAbandoned}

func (v TaskLifecycle) valid() bool {
	switch v {
	case TaskActive, TaskFinished, TaskStopped, TaskAbandoned:
		return true
	default:
		return false
	}
}
