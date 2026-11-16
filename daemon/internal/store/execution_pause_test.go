package store_test

import (
	"context"
	"testing"

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
