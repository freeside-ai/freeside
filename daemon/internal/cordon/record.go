package cordon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// A launch's records live in its ctl directory, which no launch can read or
// write, so each says only what the daemon or its supervisor wrote. Each is
// created once and never replaced. No record names a path: every path a
// reader acts on follows from RunsRoot and the run ID (layoutFor).

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
// run's or doesn't parse. A cancellation marker outranks the recorded
// ending: a run whose cancellation was requested reads as canceled, even
// when its agent exited cleanly first, so it is never exported.
func readEndRecord(l layout, runID string) (endRecord, error) {
	var rec endRecord
	if err := readRecord(l.ended, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return endRecord{}, fmt.Errorf("%w: %s", ErrNoEndRecord, runID)
		}
		return endRecord{}, err
	}
	if rec.Version != endRecordVersion || rec.RunID != runID || !rec.EndedBy.valid() {
		return endRecord{}, fmt.Errorf("cordon: end record for %s is not this run's", runID)
	}
	canceled, err := cancellationRequested(l, runID)
	if err != nil {
		return endRecord{}, err
	}
	if canceled {
		rec.EndedBy = EndedByCanceled
	}
	return rec, nil
}

// requestVersion v2 streams the program's input: v1 gave it ctl/prompt
// as plain stdin.
const requestVersion = "freeside.cordon.supervise/v2"

// supervisedProcess is one process a supervisor starts, already confined
// (confinedArgv) by the daemon that wrote the request.
type supervisedProcess struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
}

// supervisorRequest is ctl/request.json: everything a supervisor runs, so
// it needs nothing from the daemon but the run's directory and, for a
// writer, the token's pipe on tokenFD. The daemon writes it after the seed
// and before it starts the supervisor.
type supervisorRequest struct {
	Version string `json:"version"`
	RunID   string `json:"run_id"`
	// Repository, BaseRef, and BaseSHA go into the end record; a probe
	// launch, which is never exported, has none.
	Repository string            `json:"repository"`
	BaseRef    string            `json:"base_ref"`
	BaseSHA    string            `json:"base_sha"`
	Canary     supervisedProcess `json:"canary"`
	Program    supervisedProcess `json:"program"`
	// Prompt streams the program's input: ctl/prompt as the first message,
	// then what the daemon leaves in the inbox (inbox.go). Without it the
	// program's stdin is /dev/null. Secret gives it the supervisor's
	// tokenFD as its own.
	Prompt          bool  `json:"prompt"`
	Secret          bool  `json:"secret"`
	StallLimit      int64 `json:"stall_limit_ns"`
	EndGrace        int64 `json:"end_grace_ns"`
	EndProofTimeout int64 `json:"end_proof_timeout_ns"`
}

func (r supervisorRequest) check(runID string) error {
	if r.Version != requestVersion || r.RunID != runID {
		return fmt.Errorf("cordon: the supervisor request is not %s's", runID)
	}
	if len(r.Canary.Argv) == 0 || len(r.Program.Argv) == 0 {
		return errors.New("cordon: the supervisor request names no program")
	}
	if r.StallLimit <= 0 || r.EndGrace <= 0 || r.EndProofTimeout <= 0 {
		return errors.New("cordon: the supervisor request has a non-positive limit")
	}
	return nil
}

const claimVersion = "freeside.cordon.claim/v1"

// supervisorClaim is ctl/supervisor.json, the one decision of who owns a
// run: its supervisor, which claims before it starts anything, or recovery,
// which claims a run no supervisor claimed so that none ever starts it.
// Whoever creates it first owns the run.
type supervisorClaim struct {
	Version string `json:"version"`
	RunID   string `json:"run_id"`
	// Supervisor is the supervisor's pid and start time, so nothing
	// signals or waits on a later process that reuses its pid. It is null
	// in recovery's claim.
	Supervisor *claimedProcess `json:"supervisor"`
}

type claimedProcess struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start_us"`
}

func (p claimedProcess) hostProcess() hostProcess {
	return hostProcess{pid: p.PID, start: p.Start, settled: true}
}

// claimRun writes runID's claim, naming supervisor or, when it is nil,
// recovery. won is false when the run was already claimed.
func claimRun(l layout, runID string, supervisor *claimedProcess) (won bool, _ error) {
	err := writeRecord(l.claim, supervisorClaim{Version: claimVersion, RunID: runID, Supervisor: supervisor})
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	return err == nil, err
}

// readClaim reads runID's claim. A missing claim's error matches
// os.ErrNotExist.
func readClaim(l layout, runID string) (supervisorClaim, error) {
	var c supervisorClaim
	if err := readRecord(l.claim, &c); err != nil {
		return supervisorClaim{}, err
	}
	if c.Version != claimVersion || c.RunID != runID || (c.Supervisor != nil && (c.Supervisor.PID <= 1 || c.Supervisor.Start <= 0)) {
		return supervisorClaim{}, fmt.Errorf("cordon: the claim is not %s's", runID)
	}
	return c, nil
}

const cancelVersion = "freeside.cordon.cancel/v1"

// cancelMarker is ctl/cancel.json, the durable cancellation request.
type cancelMarker struct {
	Version string `json:"version"`
	RunID   string `json:"run_id"`
}

// cancellationRequested reports whether runID's cancellation marker exists.
func cancellationRequested(l layout, runID string) (bool, error) {
	var m cancelMarker
	err := readRecord(l.cancel, &m)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if m.Version != cancelVersion || m.RunID != runID {
		return false, fmt.Errorf("cordon: the cancellation marker is not %s's", runID)
	}
	return true, nil
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
// directory with every fixed member's bytes matching its export record.
// The stage driver's cordon port refuses a cordoned run's export for good
// on ErrExportUnauthenticated rather than retrying it, and its own path
// check accepts only this run's export directory. The recorded digests are
// only compared: they never choose what is read or removed.
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
	// readRecord refuses more than this, so never write what can't be read.
	if len(body) > maxRecordBytes {
		return fmt.Errorf("cordon: %s would exceed %d bytes", path, maxRecordBytes)
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
