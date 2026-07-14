package fake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// The on-disk file names, one per fake, under the fake's persistence dir.
const (
	stageStateFile  = "stage_state.json"
	reviewStateFile = "review_state.json"
)

// stageState is the durable half of a StageDriver, the three facets a daemon
// restart must preserve (§5.3, §5.9): the scripted scenarios (the external
// reality), the committed invocation intents (the outbox record: one per id),
// and the committed-result registry. Live session progress is deliberately
// absent, it is the transient provider session a restart loses.
type stageState struct {
	Scripts   map[domain.InvocationID]StageScript      `json:"scripts"`
	Committed map[domain.InvocationID]exec.StageResult `json:"committed"`
	Intents   map[domain.InvocationID]exec.StartSpec   `json:"intents"`
}

// reviewState is the durable half of a ReviewSource, mirroring stageState.
type reviewState struct {
	Scripts   map[domain.InvocationID]ReviewScript       `json:"scripts"`
	Committed map[domain.InvocationID]exec.ReviewResult  `json:"committed"`
	Intents   map[domain.InvocationID]exec.ReviewRequest `json:"intents"`
}

// loadStageState reads dir/stage_state.json, returning empty (non-nil) maps
// when the file does not exist yet: NewStageDriverAt is load-or-create, like
// store.Open. Absent maps in a partial file are normalized to empty so every
// caller writes into a live map.
func loadStageState(dir string) (stageState, error) {
	st := stageState{}
	if err := loadState(dir, stageStateFile, &st); err != nil {
		return stageState{}, err
	}
	if st.Scripts == nil {
		st.Scripts = map[domain.InvocationID]StageScript{}
	}
	if st.Committed == nil {
		st.Committed = map[domain.InvocationID]exec.StageResult{}
	}
	if st.Intents == nil {
		st.Intents = map[domain.InvocationID]exec.StartSpec{}
	}
	return st, nil
}

// loadReviewState mirrors loadStageState for the review fake.
func loadReviewState(dir string) (reviewState, error) {
	st := reviewState{}
	if err := loadState(dir, reviewStateFile, &st); err != nil {
		return reviewState{}, err
	}
	if st.Scripts == nil {
		st.Scripts = map[domain.InvocationID]ReviewScript{}
	}
	if st.Committed == nil {
		st.Committed = map[domain.InvocationID]exec.ReviewResult{}
	}
	if st.Intents == nil {
		st.Intents = map[domain.InvocationID]exec.ReviewRequest{}
	}
	return st, nil
}

// loadState unmarshals dir/name into v, treating a missing file as empty
// (load-or-create). It is the shared read half of the two typed loaders.
func loadState(dir, name string, v any) error {
	path := filepath.Join(dir, name)
	// G304: path derives from a caller-supplied fixture/daemon dir, never
	// from external input.
	b, err := os.ReadFile(path) //nolint:gosec // path is a caller-controlled fixture/daemon dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fake: read state %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("fake: parse state %s: %w", path, err)
	}
	return nil
}

// atomicWrite serializes v as indented JSON and replaces dir/name atomically:
// write a temp sibling, then rename over the target, so a reader (including
// one after a crash mid-write, the exact boundary these fakes model) never
// observes a partial file. Clock-free by construction: nothing here stamps a
// time, and encoding/json sorts map keys, so equal state marshals to
// byte-identical output on every platform.
func atomicWrite(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("fake: create state dir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("fake: marshal %s: %w", name, err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("fake: create temp for %s: %w", name, err)
	}
	tmpName := tmp.Name()
	// Cleans up the temp on any error path; a no-op after a successful rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fake: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fake: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("fake: rename %s: %w", name, err)
	}
	return nil
}
