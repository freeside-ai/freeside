package cordon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/credentialpattern"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/exportscan"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

var (
	// ErrNotExportable refuses a launch whose end record says it was
	// canceled, stalled, or exited nonzero: only a clean exit exports.
	ErrNotExportable = errors.New("cordon: the launch did not exit cleanly")
	// ErrWorktreeReplaced refuses a launch that left run/ or run/worktree
	// something other than the directory Launch seeded, such as a symlink
	// to a directory outside the run.
	ErrWorktreeReplaced = errors.New("cordon: the worktree is not the launch's own directory")
	// ErrAlreadyExported refuses a second export of one launch.
	ErrAlreadyExported = errors.New("cordon: the launch is already exported")
	// ErrExportUnauthenticated refuses a released export directory that
	// isn't the one Export recorded for the run.
	ErrExportUnauthenticated = errors.New("cordon: the export does not match its record")
)

// ScanRefusalError is an export the credential scan refused. Reason names
// the rule and the exported item, never the matched text. It matches
// ward.ErrOutputScanRefused, as the ward's check 7 refusal does.
type ScanRefusalError struct{ Reason string }

func (e *ScanRefusalError) Error() string { return "cordon: " + e.Reason }

func (e *ScanRefusalError) Unwrap() error { return ward.ErrOutputScanRefused }

// ExportRequest is the export of one launch.
type ExportRequest struct {
	RunID string
	// Transcript declares the transcript's evidence entry. Export copies
	// the launch's transcript, the agent's stdout, to Transcript.Path,
	// which must be a direct child of export.EvidenceWorkspaceDir,
	// replacing whatever the agent left there.
	Transcript export.EvidenceSource
	// Sources are files the agent may have staged, such as its summary.
	// Each is declared only when a regular file sits at its Path when
	// Export runs; the agent controls whether it exists and what it says,
	// never its label or provenance.
	Sources []export.EvidenceSource
}

// ExportResult is a launch's export. Its fields are the ones the stage
// driver takes from a warded handoff.
type ExportResult struct {
	// Dir holds manifest.json and its blobs, evidence.json and its blobs,
	// and the commit plan when the agent wrote one.
	Dir               string
	Manifest          export.Manifest
	Evidence          export.EvidenceManifest
	EvidencePresent   bool
	CommitPlanPresent bool
	// BaseSHA is the commit the launch was seeded from, read from its end
	// record, never from the worktree.
	BaseSHA string
}

// Export exports a launch's worktree with the trusted exporter, once the
// launch has a clean, proven end. It is the host's stand-in for the ward's
// networkless exporter VM over a read-only remount:
//
//   - Nothing of the run is alive: it refuses without the end record Launch
//     writes only after the run-end proof passed.
//   - Nothing else writes the tree: every launch's rules deny writes under
//     RunsRoot outside its own RUN_ROOT, and the output lands outside it.
//   - Nothing from the worktree runs: the exporter is the export package's
//     walk, in this process, and starts no git and no other program.
//
// Because it reads with the daemon's access rather than a VM's, it refuses
// a worktree the launch replaced with a symlink (ErrWorktreeReplaced) and
// any regular file with more than one hard link (export.ErrLinkedFile);
// either could name files outside the run.
//
// Before exporting, the daemon writes the evidence descriptor itself,
// replacing any the agent wrote, and the transcript beside it. A refusal or
// failure leaves no export record, so AuthenticateReleasedExport refuses
// whatever it wrote; removing it is restart recovery's (#3468). An error
// other than a ScanRefusalError can carry a path the agent chose, so a
// caller screens it before it reaches a card, as it screens a warded
// handoff's failure.
func (b *Backend) Export(ctx context.Context, req ExportRequest) (ExportResult, error) {
	if err := req.validate(); err != nil {
		return ExportResult{}, err
	}
	l := layoutFor(b.cfg.RunsRoot, req.RunID)
	end, err := readEndRecord(l, req.RunID)
	if err != nil {
		return ExportResult{}, err
	}
	if end.EndedBy != EndedByExited || end.ExitCode != 0 {
		return ExportResult{}, fmt.Errorf("%w: %s %s with exit code %d", ErrNotExportable, req.RunID, end.EndedBy, end.ExitCode)
	}
	if _, err := os.Lstat(l.exportRecord); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return ExportResult{}, fmt.Errorf("cordon: inspect %s: %w", l.exportRecord, err)
		}
		return ExportResult{}, fmt.Errorf("%w: %s", ErrAlreadyExported, req.RunID)
	}
	root, err := openWorktree(l)
	if err != nil {
		return ExportResult{}, err
	}
	defer func() { _ = root.Close() }()
	// Creating the export directory is the claim: of two exports of one
	// run, only one gets past it.
	if err := os.Mkdir(l.export, 0o750); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ExportResult{}, fmt.Errorf("%w: %s", ErrAlreadyExported, req.RunID)
		}
		return ExportResult{}, fmt.Errorf("cordon: create %s: %w", l.export, err)
	}
	if err := stageEvidence(root, l, req); err != nil {
		return ExportResult{}, err
	}
	opts := export.DefaultOptions()
	opts.RefuseLinkedFiles = true
	manifest, err := export.Export(root.FS(), l.export, opts)
	if err != nil {
		return ExportResult{}, fmt.Errorf("cordon: export %s: %w", req.RunID, err)
	}
	res := ExportResult{Dir: l.export, Manifest: manifest, BaseSHA: end.BaseSHA}
	rec := exportRecord{Version: exportRecordVersion, RunID: req.RunID}
	if rec.Manifest, _, err = digestMember(l.export, export.ManifestFilename); err != nil {
		return ExportResult{}, err
	}
	evidence, err := readMember(l.export, export.EvidenceFilename)
	if err != nil {
		return ExportResult{}, err
	}
	if evidence != nil {
		res.EvidencePresent, rec.Evidence = true, ptr(contentaddr.Sum(evidence))
		if res.Evidence, err = export.DecodeEvidenceManifest(evidence); err != nil {
			return ExportResult{}, fmt.Errorf("cordon: decode the exported evidence: %w", err)
		}
	}
	planDigest, planPresent, err := digestMember(l.export, export.CommitPlanFilename)
	if err != nil {
		return ExportResult{}, err
	}
	if planPresent {
		res.CommitPlanPresent, rec.CommitPlan = true, &planDigest
	}
	if err := (exportscan.Scanner{}).Scan(ctx, l.export); err != nil {
		var refusal *ward.OutputScanRefusal
		if errors.As(err, &refusal) {
			return ExportResult{}, &ScanRefusalError{Reason: scanRefusalReason(refusal, res)}
		}
		return ExportResult{}, fmt.Errorf("cordon: scan the export: %w", err)
	}
	if err := syncTree(l.export); err != nil {
		return ExportResult{}, fmt.Errorf("cordon: make the export durable: %w", err)
	}
	if err := writeRecord(l.exportRecord, rec); err != nil {
		return ExportResult{}, err
	}
	return res, nil
}

func (r ExportRequest) validate() error {
	if !runIDPattern.MatchString(r.RunID) || strings.Contains(r.RunID, "..") {
		return fmt.Errorf("cordon: run id %q must be one plain path component", r.RunID)
	}
	if path.Dir(r.Transcript.Path) != export.EvidenceWorkspaceDir {
		return fmt.Errorf("cordon: transcript path %q must sit directly under %s", r.Transcript.Path, export.EvidenceWorkspaceDir)
	}
	paths := map[string]bool{}
	for _, s := range append([]export.EvidenceSource{r.Transcript}, r.Sources...) {
		if paths[s.Path] {
			return fmt.Errorf("cordon: evidence path %q is declared twice", s.Path)
		}
		paths[s.Path] = true
	}
	// The descriptor's own validation refuses a bad source, a repeated
	// label, and the descriptor's path.
	_, err := export.DecodeEvidenceSourceManifest(descriptorBytes(append([]export.EvidenceSource{r.Transcript}, r.Sources...)))
	if err != nil {
		return fmt.Errorf("cordon: evidence sources: %w", err)
	}
	return nil
}

// openWorktree opens the launch's worktree only as the real directory at
// run/worktree. The launch could write RUN_ROOT itself, so it could have
// left run/ or run/worktree a symlink to any directory the account reads,
// and os.OpenRoot follows a symlink in the path it opens. The run's own
// directory sits outside RUN_ROOT, so it is the trusted starting point:
// neither component may be a symlink, and the opened handle must be the
// directory inspected.
func openWorktree(l layout) (*os.Root, error) {
	run, err := os.OpenRoot(filepath.Dir(l.runRoot))
	if err != nil {
		return nil, fmt.Errorf("cordon: open the run directory: %w", err)
	}
	defer func() { _ = run.Close() }()
	name := filepath.Base(l.runRoot)
	var inspected fs.FileInfo
	for _, p := range []string{name, filepath.Join(name, filepath.Base(l.worktree))} {
		if inspected, err = run.Lstat(p); err != nil {
			return nil, fmt.Errorf("cordon: inspect %s: %w", p, err)
		}
		if !inspected.IsDir() {
			return nil, fmt.Errorf("%w: %s is not a directory", ErrWorktreeReplaced, p)
		}
	}
	root, err := run.OpenRoot(filepath.Join(name, filepath.Base(l.worktree)))
	if err != nil {
		return nil, fmt.Errorf("cordon: open the worktree: %w", err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, inspected) {
		_ = root.Close()
		return nil, fmt.Errorf("%w: it changed while it was opened", ErrWorktreeReplaced)
	}
	return root, nil
}

// stageEvidence makes the worktree's evidence directory hold the
// transcript and the daemon's descriptor. No process of the run is alive,
// so nothing races these writes; root keeps every one inside the worktree.
func stageEvidence(root *os.Root, l layout, req ExportRequest) error {
	info, err := root.Lstat(export.EvidenceWorkspaceDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("cordon: inspect the evidence directory: %w", err)
	case !info.IsDir():
		// A symlink or file the agent put there is removed, never followed.
		if err := root.Remove(export.EvidenceWorkspaceDir); err != nil {
			return fmt.Errorf("cordon: replace the evidence directory: %w", err)
		}
		err = os.ErrNotExist
	}
	if err != nil {
		if err := root.Mkdir(export.EvidenceWorkspaceDir, 0o700); err != nil {
			return fmt.Errorf("cordon: create the evidence directory: %w", err)
		}
	}
	transcript, err := os.Open(l.transcript)
	if err != nil {
		return fmt.Errorf("cordon: open the transcript: %w", err)
	}
	defer func() { _ = transcript.Close() }()
	if err := replaceFile(root, req.Transcript.Path, transcript); err != nil {
		return err
	}
	sources := []export.EvidenceSource{req.Transcript}
	for _, s := range req.Sources {
		if info, err := root.Lstat(s.Path); err == nil && info.Mode().IsRegular() {
			sources = append(sources, s)
		}
	}
	return replaceFile(root, export.EvidenceDescriptorPath, strings.NewReader(string(descriptorBytes(sources))))
}

func descriptorBytes(sources []export.EvidenceSource) []byte {
	body, err := json.Marshal(export.EvidenceSourceManifest{Version: export.EvidenceSourceVersion, Sources: sources})
	if err != nil {
		panic(fmt.Sprintf("marshal evidence descriptor: %v", err))
	}
	return body
}

// replaceFile removes whatever the agent left at name, a directory
// included, and creates a fresh file there holding body's bytes.
func replaceFile(root *os.Root, name string, body io.Reader) error {
	if err := root.RemoveAll(name); err != nil {
		return fmt.Errorf("cordon: clear %s: %w", name, err)
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("cordon: create %s: %w", name, err)
	}
	_, err = io.Copy(f, body)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cordon: write %s: %w", name, err)
	}
	return nil
}

// syncTree makes every file and directory entry under dir durable, children
// before their parents.
func syncTree(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		f, err := os.Open(p) //nolint:gosec // G304: the export the daemon just wrote
		if err != nil {
			return err
		}
		return errors.Join(f.Sync(), f.Close())
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := atomicfile.SyncDir(dirs[i]); err != nil {
			return err
		}
	}
	return atomicfile.SyncDir(filepath.Dir(dir))
}

// scanRefusalReason says what a refusal matched without the match, as the
// ward's does: a rule outside credentialpattern.All() and a file outside
// the export's layout read as unnamed, and a name a rule matches shows
// only as its redacted token.
func scanRefusalReason(refusal *ward.OutputScanRefusal, res ExportResult) string {
	rule := "an unnamed credential rule"
	for _, known := range credentialpattern.All() {
		if refusal.Rule == known.ID {
			rule = "credential rule " + known.ID
			break
		}
	}
	return fmt.Sprintf("output scan refused the export: %s matched %s", scannedItem(refusal.File, res), rule)
}

func scannedItem(file string, res ExportResult) string {
	switch file {
	case export.ManifestFilename:
		return "the repo manifest"
	case export.EvidenceFilename:
		return "the evidence manifest"
	case export.CommitPlanFilename:
		return "the commit plan"
	}
	var names []string
	if hexDigest, ok := strings.CutPrefix(file, "blobs/sha256/"); ok {
		for _, e := range res.Manifest.Entries {
			if e.Kind == export.EntryRegular && !e.BlobOmitted && contentaddr.Hex(string(*e.Digest)) == hexDigest {
				names = append(names, e.Path)
			}
		}
		if len(names) > 0 {
			return "worktree file " + namedWithOthers(names)
		}
	}
	if hexDigest, ok := strings.CutPrefix(file, export.EvidenceBlobsDirname+"/sha256/"); ok {
		for _, e := range res.Evidence.Entries {
			if contentaddr.Hex(string(e.Digest)) == hexDigest {
				names = append(names, e.Label)
			}
		}
		if len(names) > 0 {
			return "evidence item " + namedWithOthers(names)
		}
	}
	return "an unnamed exported file"
}

func namedWithOthers(names []string) string {
	shown := strconv.Quote(names[0])
	for _, rule := range credentialpattern.All() {
		if rule.Pattern.MatchString(names[0]) {
			shown = domain.RedactedName(names[0])
			break
		}
	}
	if len(names) == 1 {
		return shown
	}
	return fmt.Sprintf("%s (and %d more with the same content)", shown, len(names)-1)
}

func ptr[T any](v T) *T { return &v }
