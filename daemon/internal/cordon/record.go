package cordon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/export"
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

const exportRecordVersion = "freeside.cordon.export/v1"

// exportRecord binds a launch's released export to the bytes Export wrote:
// the digest of each fixed member, null for a member that is absent. The
// importer checks every blob against the manifest, so binding the three
// members binds the whole export.
type exportRecord struct {
	Version    string  `json:"version"`
	RunID      string  `json:"run_id"`
	Manifest   string  `json:"manifest_sha256"`
	Evidence   *string `json:"evidence_sha256"`
	CommitPlan *string `json:"commit_plan_sha256"`
}

// AuthenticateReleasedExport accepts exportDir only as runID's export
// directory with every fixed member's bytes matching its export record. It
// has stage.Gate's signature only: the stage pipeline's released-export
// path check accepts only the ward's handoff directories, and its retry
// classification would retry ErrExportUnauthenticated rather than refuse
// for good, so the driver port that wires this in changes both. The
// recorded digests are only compared: they never choose what is read or
// removed.
func (b *Backend) AuthenticateReleasedExport(ctx context.Context, runID, exportDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !runIDPattern.MatchString(runID) || strings.Contains(runID, "..") {
		return fmt.Errorf("%w: run id %q is not one plain path component", ErrExportUnauthenticated, runID)
	}
	l := layoutFor(b.cfg.RunsRoot, runID)
	if exportDir != l.export {
		return fmt.Errorf("%w: %s is not %s's export directory", ErrExportUnauthenticated, exportDir, runID)
	}
	var rec exportRecord
	if err := readRecord(l.exportRecord, &rec); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrExportUnauthenticated, runID, err)
	}
	if rec.Version != exportRecordVersion || rec.RunID != runID {
		return fmt.Errorf("%w: the export record is not %s's", ErrExportUnauthenticated, runID)
	}
	members := []struct {
		name string
		want *string
	}{
		{export.ManifestFilename, &rec.Manifest},
		{export.EvidenceFilename, rec.Evidence},
		{export.CommitPlanFilename, rec.CommitPlan},
	}
	for _, m := range members {
		got, present, err := digestMember(exportDir, m.name)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrExportUnauthenticated, err)
		}
		if present != (m.want != nil) || (present && got != *m.want) {
			return fmt.Errorf("%w: %s differs from the export record", ErrExportUnauthenticated, m.name)
		}
	}
	return nil
}

// openMember opens one fixed member of an export directory, which must be
// a regular file. It returns nil for an absent member.
func openMember(dir, name string) (*os.File, error) {
	p := filepath.Join(dir, name)
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cordon: inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("cordon: %s is not a regular file", name)
	}
	f, err := os.Open(p) //nolint:gosec // G304: a fixed member of the run's export directory
	if err != nil {
		return nil, fmt.Errorf("cordon: open %s: %w", name, err)
	}
	return f, nil
}

// digestMember returns a member's content address, streaming it, and
// whether it is present.
func digestMember(dir, name string) (string, bool, error) {
	f, err := openMember(dir, name)
	if err != nil || f == nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false, fmt.Errorf("cordon: read %s: %w", name, err)
	}
	return contentaddr.Format(h.Sum(nil)), true, nil
}

// readMember returns a member's bytes, or nil when it is absent.
func readMember(dir, name string) ([]byte, error) {
	f, err := openMember(dir, name)
	if err != nil || f == nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("cordon: read %s: %w", name, err)
	}
	return body, nil
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
