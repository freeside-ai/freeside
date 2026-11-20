// Package readyshare reports how many tasks reached a ready pull request
// without raising any attention item, and which items the others raised
// first, by type and interruption class (issue #3406). Its ranking shows which
// recurring items to make autonomous next.
//
// It is pure: the operator command reads the stored items through the store
// and hands them here, and nothing in this package reaches the store. The
// report is advisory (Tier C) and grants nothing.
package readyshare

import (
	"cmp"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/causecount"
)

// Item is one stored attention item. An empty TaskID means the item names no
// task. Type and Class are the stored strings, so a value this build doesn't
// know is still counted.
type Item struct {
	ItemID    domain.ItemID
	TaskID    domain.TaskID
	Type      string
	Class     string
	CreatedAt time.Time
}

// ClassCount tallies the before-ready items of one interruption class: how
// many items, and how many distinct counted tasks raised at least one.
type ClassCount struct {
	Class string `json:"interruption_class"`
	Known bool   `json:"known"`
	Items int    `json:"items"`
	Tasks int    `json:"tasks"`
}

// TypeCount tallies the before-ready items of one (type, class) pair. Known is
// false for a type this build doesn't know.
type TypeCount struct {
	Type  string `json:"type"`
	Class string `json:"interruption_class"`
	Known bool   `json:"known"`
	Items int    `json:"items"`
	Tasks int    `json:"tasks"`
}

// ItemRef is one item a task raised before it reached ready.
type ItemRef struct {
	ItemID    domain.ItemID `json:"item_id"`
	Type      string        `json:"type"`
	Class     string        `json:"interruption_class"`
	CreatedAt time.Time     `json:"created_at"`
}

// Task is one task that reached ready in the period: its first ready item,
// when it was created, and the items the task raised before it.
type Task struct {
	TaskID      domain.TaskID `json:"task_id"`
	ReadyItemID domain.ItemID `json:"ready_item_id"`
	ReadyAt     time.Time     `json:"ready_at"`
	Items       []ItemRef     `json:"items"`
}

// TaskLessCount tallies the items with no task created in the period, by
// (type, class). They belong to no task, so they never change the share.
type TaskLessCount struct {
	Type  string `json:"type"`
	Class string `json:"interruption_class"`
	Known bool   `json:"known"`
	Items int    `json:"items"`
}

// Report is the printed shape of `freesided ready-share`. ProjectID is nil
// when every project counts; ShareWithoutItems is nil when no task reached
// ready in the period.
type Report struct {
	ProjectID              *domain.ProjectID    `json:"project_id"`
	Period                 causecount.Window    `json:"period"`
	TasksReady             int                  `json:"tasks_ready"`
	TasksReadyWithoutItems int                  `json:"tasks_ready_without_items"`
	ShareWithoutItems      *float64             `json:"share_without_items"`
	ByClass                []ClassCount         `json:"by_class"`
	ByType                 []TypeCount          `json:"by_type"`
	Tasks                  []Task               `json:"tasks"`
	TaskLess               []TaskLessCount      `json:"task_less"`
	Skipped                []causecount.Skipped `json:"skipped"`
}

// Compute counts the tasks whose first ready_for_final_review item was
// created in period, and the items each raised strictly before that item. It
// leaves ProjectID and Skipped for the caller, which knows them.
//
//   - A task's ready time is the earliest created_at among its ready items,
//     ties broken by item ID; a later ready item, from another cycle, never
//     moves it.
//   - A before-ready item is any other item of the task created strictly
//     before the ready time. The ready item, and everything at or after it,
//     is excluded.
//   - A task-less item counts in TaskLess when it was created in period.
//
// ByClass lists every domain.InterruptionClass, in its declared order, then
// any unknown stored class by name. ByType ranks by tasks, then items, then
// type and class. Tasks are ordered by ready time, then task ID, and each
// task's items by creation time, then item ID.
func Compute(items []Item, period causecount.Window) Report {
	byTask := map[domain.TaskID][]Item{}
	taskLess := map[typeClass]int{}
	for _, item := range items {
		if item.TaskID == "" {
			if inWindow(period, item.CreatedAt) {
				taskLess[typeClass{item.Type, item.Class}]++
			}
			continue
		}
		byTask[item.TaskID] = append(byTask[item.TaskID], item)
	}

	report := Report{Period: period, Tasks: []Task{}}
	for taskID, taskItems := range byTask {
		ready, ok := firstReady(taskItems)
		if !ok || !inWindow(period, ready.CreatedAt) {
			continue
		}
		task := Task{TaskID: taskID, ReadyItemID: ready.ItemID, ReadyAt: ready.CreatedAt, Items: []ItemRef{}}
		for _, item := range taskItems {
			if item.CreatedAt.Before(ready.CreatedAt) {
				task.Items = append(task.Items, ItemRef{
					ItemID: item.ItemID, Type: item.Type, Class: item.Class, CreatedAt: item.CreatedAt,
				})
			}
		}
		slices.SortFunc(task.Items, func(a, b ItemRef) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ItemID, b.ItemID))
		})
		report.Tasks = append(report.Tasks, task)
	}
	slices.SortFunc(report.Tasks, func(a, b Task) int {
		return cmp.Or(a.ReadyAt.Compare(b.ReadyAt), cmp.Compare(a.TaskID, b.TaskID))
	})

	report.TasksReady = len(report.Tasks)
	for _, task := range report.Tasks {
		if len(task.Items) == 0 {
			report.TasksReadyWithoutItems++
		}
	}
	if report.TasksReady > 0 {
		share := float64(report.TasksReadyWithoutItems) / float64(report.TasksReady)
		report.ShareWithoutItems = &share
	}
	report.ByClass = countByClass(report.Tasks)
	report.ByType = countByType(report.Tasks)
	report.TaskLess = countTaskLess(taskLess)
	return report
}

type typeClass struct{ itemType, class string }

// tally counts items and the distinct tasks that raised them.
type tally struct {
	items int
	tasks map[domain.TaskID]bool
}

func (t *tally) add(task domain.TaskID) {
	t.items++
	t.tasks[task] = true
}

func newTally() *tally { return &tally{tasks: map[domain.TaskID]bool{}} }

// firstReady is the task's earliest ready_for_final_review item, ties broken
// by item ID so the choice doesn't depend on read order.
func firstReady(items []Item) (Item, bool) {
	var first Item
	found := false
	for _, item := range items {
		if item.Type != string(domain.AttentionReadyForFinalReview) {
			continue
		}
		if !found || cmp.Or(item.CreatedAt.Compare(first.CreatedAt), cmp.Compare(item.ItemID, first.ItemID)) < 0 {
			first, found = item, true
		}
	}
	return first, found
}

func countByClass(tasks []Task) []ClassCount {
	tallies := map[string]*tally{}
	for _, class := range domain.AllInterruptionClasses {
		tallies[string(class)] = newTally()
	}
	for _, task := range tasks {
		for _, item := range task.Items {
			t := tallies[item.Class]
			if t == nil {
				t = newTally()
				tallies[item.Class] = t
			}
			t.add(task.TaskID)
		}
	}
	out := make([]ClassCount, 0, len(tallies))
	for class, t := range tallies {
		out = append(out, ClassCount{Class: class, Known: knownClass(class), Items: t.items, Tasks: len(t.tasks)})
	}
	slices.SortFunc(out, func(a, b ClassCount) int {
		return cmp.Or(cmp.Compare(classRank(a.Class), classRank(b.Class)), cmp.Compare(a.Class, b.Class))
	})
	return out
}

func countByType(tasks []Task) []TypeCount {
	tallies := map[typeClass]*tally{}
	for _, task := range tasks {
		for _, item := range task.Items {
			k := typeClass{item.Type, item.Class}
			t := tallies[k]
			if t == nil {
				t = newTally()
				tallies[k] = t
			}
			t.add(task.TaskID)
		}
	}
	out := make([]TypeCount, 0, len(tallies))
	for k, t := range tallies {
		out = append(out, TypeCount{
			Type: k.itemType, Class: k.class, Known: knownType(k.itemType), Items: t.items, Tasks: len(t.tasks),
		})
	}
	slices.SortFunc(out, func(a, b TypeCount) int {
		return cmp.Or(
			cmp.Compare(b.Tasks, a.Tasks), cmp.Compare(b.Items, a.Items),
			cmp.Compare(a.Type, b.Type), cmp.Compare(a.Class, b.Class))
	})
	return out
}

func countTaskLess(items map[typeClass]int) []TaskLessCount {
	out := make([]TaskLessCount, 0, len(items))
	for k, n := range items {
		out = append(out, TaskLessCount{Type: k.itemType, Class: k.class, Known: knownType(k.itemType), Items: n})
	}
	slices.SortFunc(out, func(a, b TaskLessCount) int {
		return cmp.Or(cmp.Compare(b.Items, a.Items), cmp.Compare(a.Type, b.Type), cmp.Compare(a.Class, b.Class))
	})
	return out
}

func inWindow(w causecount.Window, at time.Time) bool {
	return !at.Before(w.Start) && at.Before(w.End)
}

func knownType(t string) bool {
	return slices.Contains(domain.AllAttentionTypes, domain.AttentionType(t))
}

func knownClass(c string) bool {
	return slices.Contains(domain.AllInterruptionClasses, domain.InterruptionClass(c))
}

// classRank orders the known classes as declared and every unknown one after.
func classRank(c string) int {
	if i := slices.Index(domain.AllInterruptionClasses, domain.InterruptionClass(c)); i >= 0 {
		return i
	}
	return len(domain.AllInterruptionClasses)
}
