package ward

import (
	"encoding/json"
	"errors"
	"os"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// CancellationCoverage records the tasks that predate runtime ownership
// journaling. An empty per-run journal is proof of no launch only for a task
// created after this checkpoint, in the same database epoch. Initialization
// runs under the daemon database lock before serving commands or scheduling.
type CancellationCoverage struct {
	Version        int             `json:"version"`
	Epoch          string          `json:"epoch"`
	UncoveredTasks []domain.TaskID `json:"uncovered_tasks"`
}

func OpenCancellationCoverage(path, epoch string, existing []domain.TaskID) (CancellationCoverage, error) {
	var record CancellationCoverage
	body, err := os.ReadFile(path) // #nosec G304 -- daemon-owned checkpoint path under its private state root.
	if errors.Is(err, os.ErrNotExist) {
		record = CancellationCoverage{Version: 1, Epoch: epoch, UncoveredTasks: slices.Clone(existing)}
		body, err = json.Marshal(record)
		if err == nil {
			err = atomicfile.WriteFile(path, body, 0o600)
		}
	} else if err == nil {
		err = json.Unmarshal(body, &record)
	}
	if err != nil {
		return CancellationCoverage{}, err
	}
	if record.Version != 1 || record.Epoch == "" {
		return CancellationCoverage{}, errors.New("invalid task cancellation coverage record")
	}
	return record, nil
}

func (c CancellationCoverage) Covers(task domain.TaskID, epoch string) error {
	if c.Epoch != epoch || slices.Contains(c.UncoveredTasks, task) {
		return errors.New("task predates provable runtime ownership in this database epoch")
	}
	return nil
}
