package cordon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// The launch's two records live in its ctl directory, which no launch can
// read or write, so each says only what the daemon wrote. Both are created
// once and never replaced.

// ErrNoEndRecord means a launch has no proven end: it is running, it was
// blocked, or it failed before it returned a result.
var ErrNoEndRecord = errors.New("cordon: the launch has no end record")

// maxRecordBytes bounds a record read; a real one is a few hundred bytes.
const maxRecordBytes = 64 << 10

const endRecordVersion = "freeside.cordon.end/v1"

// endRecord is how a launch ended. Launch writes it only once the run-end
// proof has passed and the agent's output is closed, as the last step
// before it returns a result, so its presence means no process of the
// launch is alive and the worktree holds still.
type endRecord struct {
	Version    string  `json:"version"`
	RunID      string  `json:"run_id"`
	Repository string  `json:"repository"`
	BaseRef    string  `json:"base_ref"`
	BaseSHA    string  `json:"base_sha"`
	EndedBy    EndedBy `json:"ended_by"`
	ExitCode   int     `json:"exit_code"`
}

func writeEndRecord(l layout, rec endRecord) error {
	rec.Version = endRecordVersion
	return writeRecord(l.ended, rec)
}

// readEndRecord reads runID's end record, refusing one that isn't this
// run's or doesn't parse.
func readEndRecord(l layout, runID string) (endRecord, error) {
	var rec endRecord
	if err := readRecord(l.ended, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return endRecord{}, fmt.Errorf("%w: %s", ErrNoEndRecord, runID)
		}
		return endRecord{}, err
	}
	if rec.Version != endRecordVersion || rec.RunID != runID || !rec.EndedBy.valid() || rec.BaseSHA == "" {
		return endRecord{}, fmt.Errorf("cordon: end record for %s is not this run's", runID)
	}
	return rec, nil
}

func writeRecord(path string, rec any) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("cordon: encode %s: %w", path, err)
	}
	if err := atomicfile.WriteFileNoReplace(path, body, 0o600); err != nil {
		return fmt.Errorf("cordon: write %s: %w", path, err)
	}
	return nil
}

// readRecord decodes a record strictly: unknown fields, duplicate names, and
// trailing data refuse. A missing record's error matches os.ErrNotExist.
func readRecord(path string, rec any) error {
	f, err := os.Open(path) //nolint:gosec // G304: a path in the launch's own control directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return fmt.Errorf("cordon: read %s: %w", path, err)
	}
	if len(body) > maxRecordBytes {
		return fmt.Errorf("cordon: %s exceeds %d bytes", path, maxRecordBytes)
	}
	if err := strictjson.Decode(body, rec, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		return fmt.Errorf("cordon: decode %s: %w", path, err)
	}
	return nil
}
