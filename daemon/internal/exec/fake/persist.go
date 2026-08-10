package fake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
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
	Failed    map[domain.InvocationID]bool               `json:"failed"`
	Rejected  map[domain.InvocationID]bool               `json:"rejected"`
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
	if st.Failed == nil {
		st.Failed = map[domain.InvocationID]bool{}
	}
	if st.Rejected == nil {
		st.Rejected = map[domain.InvocationID]bool{}
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

// atomicWrite serializes v as indented JSON and replaces dir/name durably, so
// the fake models the same restart boundary as a production driver. Clock-free
// by construction: nothing here stamps a time, and encoding/json sorts map
// keys, so equal state marshals to byte-identical output on every platform.
func atomicWrite(dir, name string, v any) error {
	if err := makeStateDirectory(dir); err != nil {
		return fmt.Errorf("fake: create state dir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("fake: marshal %s: %w", name, err)
	}
	b = append(b, '\n')

	if err := atomicfile.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		return fmt.Errorf("fake: persist %s: %w", name, err)
	}
	return nil
}

func makeStateDirectory(path string) error {
	return makeStateDirectoryWithSync(path, atomicfile.SyncDir)
}

func makeStateDirectoryWithSync(path string, syncDir func(string) error) error {
	var missing []string
	var existing string
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", current)
			}
			existing = current
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		if parent := filepath.Dir(current); parent == current {
			return fmt.Errorf("no existing ancestor for %s", path)
		}
	}
	if err := syncDir(filepath.Dir(existing)); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o750); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, statErr := os.Stat(missing[i])
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", missing[i])
			}
		}
		if err := syncDir(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}
