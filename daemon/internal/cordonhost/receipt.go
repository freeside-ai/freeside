package cordonhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// The receipt: a clean run's export crosses from the launcher as a tree
// stream (seed.go) that the client treats as untrusted input, as the ward's
// daemon side treats an exporter VM's archive. It lands in a directory the
// client chose, under exportRules' bounds, through an os.Root. The client
// then reads the manifests itself, runs the export credential scan a local
// export runs, makes the tree durable, renames it into place, and writes
// its receipt record last. A refused or cut receipt is removed and leaves
// no record, so nothing reads as an export. The importer's gates run on the
// result unchanged. The launcher's word is taken for one thing only, the
// base commit, which the stage driver compares with the run's admission.
//
// A run's receipt directory, <ExportRoot>/<runID>, holds only the names
// below, and the client removes only those names; a directory holding
// anything else is refused rather than cleared. One receiver acts on a run
// at a time: the stage driver drives each run from one place.

const (
	receivingName = ".receiving"
	exportName    = "export"
	recordName    = "receipt.json"
	// maxExportManifestBytes bounds each manifest read into memory, the
	// ward's MaxManifestBytes default (export.DefaultMaxCommitPlanBytes).
	maxExportManifestBytes = export.DefaultMaxCommitPlanBytes
	// maxReceiptRecordBytes bounds a receipt record read; a real one is a
	// few hundred bytes.
	maxReceiptRecordBytes = 64 << 10
	receiptVersion        = "freeside.cordonhost.receipt/v1"
)

var receiptNames = []string{receivingName, exportName, recordName}

// ErrExportRefused marks an export the client refuses for what it holds,
// not for how it arrived: asking again brings the same bytes, so the gate
// ends the run refused, as the ward ends one whose archive fails
// verification. A stream cut short is ErrProtocol alone, and is asked
// for again.
var ErrExportRefused = fmt.Errorf("%w: the export is not one the client accepts", ErrProtocol)

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// receiptLayout is one run's receipt paths.
type receiptLayout struct{ dir, receiving, export, record string }

func (c *Client) receipt(runID string) receiptLayout {
	dir := filepath.Join(c.cfg.ExportRoot, runID)
	return receiptLayout{
		dir: dir, receiving: filepath.Join(dir, receivingName),
		export: filepath.Join(dir, exportName), record: filepath.Join(dir, recordName),
	}
}

// receiptRecord binds a received export: its run, the base commit the
// launcher reported, and the digests of its three fixed members, which the
// client computed from the bytes it kept.
type receiptRecord struct {
	Version    string  `json:"version"`
	RunID      string  `json:"run_id"`
	BaseSHA    string  `json:"base_sha"`
	Manifest   string  `json:"manifest_sha256"`
	Evidence   *string `json:"evidence_sha256"`
	CommitPlan *string `json:"commit_plan_sha256"`
}

func (r receiptRecord) digests() cordon.ExportDigests {
	return cordon.ExportDigests{Manifest: r.Manifest, Evidence: r.Evidence, CommitPlan: r.CommitPlan}
}

// readReceiptRecord reads runID's receipt record. A missing record's error
// matches os.ErrNotExist.
func readReceiptRecord(path, runID string) (receiptRecord, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a fixed name in the client's own receipt directory
	if err != nil {
		return receiptRecord{}, err
	}
	defer func() { _ = f.Close() }()
	var rec receiptRecord
	if err := strictjson.DecodeReader(f, &rec, strictjson.RejectInvalidUTF8, strictjson.Limit(maxReceiptRecordBytes)); err != nil {
		return receiptRecord{}, fmt.Errorf("cordonhost: decode %s: %w", path, err)
	}
	if rec.Version != receiptVersion || rec.RunID != runID || !commitSHAPattern.MatchString(rec.BaseSHA) {
		return receiptRecord{}, fmt.Errorf("cordonhost: the receipt record is not %s's", runID)
	}
	for _, d := range []*string{&rec.Manifest, rec.Evidence, rec.CommitPlan} {
		if d != nil && !contentaddr.Valid(*d) {
			return receiptRecord{}, fmt.Errorf("cordonhost: the receipt record names a malformed digest")
		}
	}
	return rec, nil
}

// received returns runID's export from its receipt, when the client holds
// one whose members match its record. With no record it removes what a
// cut receipt left and reports none, so the caller asks the launcher.
func (c *Client) received(runID string) (cordon.ExportResult, bool, error) {
	l := c.receipt(runID)
	rec, err := readReceiptRecord(l.record, runID)
	if errors.Is(err, os.ErrNotExist) {
		return cordon.ExportResult{}, false, l.removeOwned()
	}
	if err != nil {
		return cordon.ExportResult{}, false, fmt.Errorf("%w: %w", cordon.ErrExportUnauthenticated, err)
	}
	if err := cordon.VerifyExportDigests(l.export, rec.digests()); err != nil {
		return cordon.ExportResult{}, false, err
	}
	res, err := readReceived(l.export)
	if err != nil {
		return cordon.ExportResult{}, false, err
	}
	res.BaseSHA = rec.BaseSHA
	return res, true, nil
}

// fetch sends req and, when the reply carries an export, receives it into
// runID's receipt. check reads the reply first; a reply it refuses ends
// the operation with its error. The export is nil when none came.
func (c *Client) fetch(ctx context.Context, req frame, runID string, check func(*replyBody) error) (*replyBody, *cordon.ExportResult, error) {
	l := c.receipt(runID)
	reply, err := c.callThen(ctx, req, func(r io.Reader, reply *replyBody) error {
		if err := check(reply); err != nil {
			return err
		}
		if reply.Export == nil {
			return nil
		}
		if !commitSHAPattern.MatchString(reply.Export.BaseSHA) {
			return fmt.Errorf("%w: an export from base %q", ErrExportRefused, boundText(reply.Export.BaseSHA))
		}
		return l.take(r)
	})
	if err != nil {
		// A refusal before the stream leaves nothing; a cut stream left a
		// partial tree.
		return nil, nil, errors.Join(err, l.discard())
	}
	if reply.Export == nil {
		return reply, nil, nil
	}
	res, err := l.commit(ctx, runID, reply.Export.BaseSHA)
	if err != nil {
		return nil, nil, err
	}
	return reply, &res, nil
}

// take extracts the stream into the receiving directory under the export's
// bounds. The receipt directory held no receiving directory: received
// cleared it before the request.
func (l receiptLayout) take(r io.Reader) error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("cordonhost: create %s: %w", l.dir, err)
	}
	// The record is synced in l.dir; l.dir's own entry must be too, or a
	// crash can lose a receipt the stage driver already recorded.
	if err := atomicfile.SyncDir(filepath.Dir(l.dir)); err != nil {
		return fmt.Errorf("cordonhost: sync %s: %w", filepath.Dir(l.dir), err)
	}
	if err := readTree(r, l.receiving, exportRules); err != nil {
		return fmt.Errorf("cordonhost: receive the export: %w", err)
	}
	return nil
}

// commit checks the received tree as an export and keeps it: the
// manifests decode within their bounds, the credential scan passes, the
// tree is durable, it is renamed into place, and the record is written
// last. Any failure removes the tree and leaves no record.
func (l receiptLayout) commit(ctx context.Context, runID, baseSHA string) (cordon.ExportResult, error) {
	res, digests, err := checkReceived(ctx, l.receiving)
	if err == nil {
		err = os.Rename(l.receiving, l.export)
	}
	if err == nil {
		err = atomicfile.SyncDir(l.dir)
	}
	if err == nil {
		err = writeReceiptRecord(l.record, receiptRecord{
			Version: receiptVersion, RunID: runID, BaseSHA: baseSHA,
			Manifest: digests.Manifest, Evidence: digests.Evidence, CommitPlan: digests.CommitPlan,
		})
	}
	if err != nil {
		return cordon.ExportResult{}, errors.Join(err, l.discard())
	}
	res.Dir, res.BaseSHA = l.export, baseSHA
	return res, nil
}

// checkReceived reads, scans, digests, and syncs a received tree. A scan
// refusal returns as the *cordon.ScanRefusalError a local export's does.
func checkReceived(ctx context.Context, dir string) (cordon.ExportResult, cordon.ExportDigests, error) {
	res, err := readReceived(dir)
	if err != nil {
		return cordon.ExportResult{}, cordon.ExportDigests{}, err
	}
	if err := cordon.ScanExportDir(ctx, res); err != nil {
		return cordon.ExportResult{}, cordon.ExportDigests{}, err
	}
	digests, err := cordon.DigestExportDir(dir)
	if err != nil {
		return cordon.ExportResult{}, cordon.ExportDigests{}, err
	}
	if err := cordon.SyncTree(dir); err != nil {
		return cordon.ExportResult{}, cordon.ExportDigests{}, fmt.Errorf("cordonhost: make the export durable: %w", err)
	}
	return res, digests, nil
}

// readReceived reads an export directory's result once its manifests are
// within the bound the ward reads them under, and validates the manifest
// as the ward's receipt does.
func readReceived(dir string) (cordon.ExportResult, error) {
	for _, name := range []string{export.ManifestFilename, export.EvidenceFilename} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return cordon.ExportResult{}, fmt.Errorf("cordonhost: inspect the export's %s: %w", name, err)
		}
		if info.Size() > maxExportManifestBytes {
			return cordon.ExportResult{}, fmt.Errorf("%w: its %s is over %d bytes", ErrExportRefused, name, maxExportManifestBytes)
		}
	}
	res, err := cordon.ReadExportDir(dir)
	if err != nil {
		return cordon.ExportResult{}, fmt.Errorf("%w: %w", ErrExportRefused, err)
	}
	// A validation error names an entry's path, which the agent chose.
	if res.Manifest.Validate() != nil {
		return cordon.ExportResult{}, fmt.Errorf("%w: its manifest failed %s validation", ErrExportRefused, export.ManifestVersion)
	}
	return res, nil
}

func writeReceiptRecord(path string, rec receiptRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("cordonhost: encode the receipt record: %w", err)
	}
	if err := atomicfile.WriteFileNoReplace(path, body, 0o600); err != nil {
		return fmt.Errorf("cordonhost: write the receipt record: %w", err)
	}
	return nil
}

// discard removes what a receipt that did not finish left: the receiving
// directory, and the export when its record was never written.
func (l receiptLayout) discard() error {
	if _, err := os.Lstat(l.record); err == nil {
		return os.RemoveAll(l.receiving)
	}
	return errors.Join(os.RemoveAll(l.receiving), os.RemoveAll(l.export))
}

// removeOwned removes every name a receipt puts in its directory, refusing
// a directory that holds anything else.
func (l receiptLayout) removeOwned() error {
	entries, err := os.ReadDir(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cordonhost: read %s: %w", l.dir, err)
	}
	for _, e := range entries {
		if !ownedName(e.Name()) {
			return fmt.Errorf("cordonhost: %s holds %q, which no receipt puts there", l.dir, e.Name())
		}
	}
	var errs []error
	for _, e := range entries {
		errs = append(errs, os.RemoveAll(filepath.Join(l.dir, e.Name())))
	}
	return errors.Join(errs...)
}

// ownedName reports whether a receipt puts name in its directory: one of
// receiptNames, or the temporary file atomicfile stages the record in,
// which a crash mid-write leaves.
func ownedName(name string) bool {
	return slices.Contains(receiptNames, name) ||
		(strings.HasPrefix(name, "."+recordName+"-") && strings.HasSuffix(name, ".tmp"))
}

// release removes the run's receipt and its directory.
func (l receiptLayout) release() error {
	if err := l.removeOwned(); err != nil {
		return err
	}
	if err := os.Remove(l.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cordonhost: remove %s: %w", l.dir, err)
	}
	return nil
}
