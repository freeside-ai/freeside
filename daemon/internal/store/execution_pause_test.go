package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestExecutionPauseReadsTheSectionAlone pins the read prod's start makes on
// a store that holds no run settings: none, then each state, then none after
// a clear.
func TestExecutionPauseReadsTheSectionAlone(t *testing.T) {
	t.Parallel()
	s := openSettingsStore(t)
	read := func() *domain.ExecutionPauseSettings {
		t.Helper()
		var pause *domain.ExecutionPauseSettings
		if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
			var err error
			pause, err = tx.ExecutionPause(context.Background())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return pause
	}
	if pause := read(); pause != nil {
		t.Fatalf("empty store: pause %+v", pause)
	}
	for _, step := range []struct {
		value string
		want  domain.ExecutionPauseState
	}{
		{`{"state":"requested"}`, domain.ExecutionPauseRequested},
		{`{"state":"released"}`, domain.ExecutionPauseReleased},
		{"", ""},
	} {
		if _, err := appendInstanceSettings(t, s, domain.InstanceSectionExecutionPause, step.value); err != nil {
			t.Fatal(err)
		}
		pause := read()
		if step.want == "" {
			if pause != nil {
				t.Fatalf("after the clear: pause %+v", pause)
			}
			continue
		}
		if pause == nil || pause.State != step.want {
			t.Fatalf("pause %+v, want %s", pause, step.want)
		}
	}
}

// TestRequireNewWorkOpenRefusesADrainOrAPause: new work is open only with
// neither a drain nor a pause in force; the drain's refusal wins when both
// are, and NewWorkClosed recognizes each refusal, wrapped or not.
func TestRequireNewWorkOpenRefusesADrainOrAPause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openSettingsStore(t)
	check := func() error {
		return s.Read(ctx, func(tx *store.ReadTx) error { return tx.RequireNewWorkOpen(ctx) })
	}
	requireClosed := func(label string, want error) {
		t.Helper()
		err := check()
		if !errors.Is(err, want) || !store.NewWorkClosed(err) {
			t.Fatalf("%s: %v, want %v", label, err, want)
		}
	}
	if err := check(); err != nil || store.NewWorkClosed(err) {
		t.Fatalf("open: %v", err)
	}
	if _, err := appendInstanceSettings(t, s, domain.InstanceSectionExecutionPause, `{"state":"requested"}`); err != nil {
		t.Fatal(err)
	}
	requireClosed("paused", store.ErrExecutionPauseInForce)
	recordDrain(t, s, domain.UpdateDrainTransition{
		State: domain.UpdateDrainDraining, Build: drainBuild, OccurredAt: admissionEpoch,
	})
	requireClosed("paused and draining", store.ErrUpdateDrainInForce)
	if _, err := appendInstanceSettings(t, s, domain.InstanceSectionExecutionPause, ""); err != nil {
		t.Fatal(err)
	}
	requireClosed("draining", store.ErrUpdateDrainInForce)
	recordDrain(t, s, domain.UpdateDrainTransition{
		State: domain.UpdateDrainLifted, Build: drainBuild, OccurredAt: admissionEpoch.Add(time.Minute),
	})
	if err := check(); err != nil {
		t.Fatalf("after both ended: %v", err)
	}
	if store.NewWorkClosed(errors.New("other")) || !store.NewWorkClosed(fmt.Errorf("x: %w", store.ErrExecutionPauseInForce)) {
		t.Fatal("NewWorkClosed misclassifies")
	}
}
