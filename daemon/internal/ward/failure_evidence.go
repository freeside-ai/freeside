package ward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

const (
	// Diagnostics are bounded independently of a potentially damaged source tree.
	MaxFailureTranscriptBytes = 4 << 20
	failureDescriptorBytes    = 1 << 20
	failureDescriptorProof    = "/freeside-failure-descriptor.json"
	failureTranscriptProof    = "/freeside-failure-transcript.txt"
)

// FailureEvidence is diagnostic output only. It confers no workspace or
// publication authority. The body may contain untrusted provider text.
type FailureEvidence struct {
	Body        []byte
	Digest      string
	Unavailable bool
}

func writerOutcomeObserverCommand(workspace string, hs HandoffSpec) string {
	script := "set -eu; cat " + shellQuote(hs.Agent.OutcomeMarkerPath) +
		" > " + shellQuote(writerOutcomeProofPath) + "; "
	if source := hs.Agent.FailureTranscript; source != nil {
		root := path.Join(workspace, export.EvidenceWorkspaceDir)
		// The writer is gone and the volume is read-only. Check every component
		// before reading; cap copies inside the observer, then check again on host.
		script += "if [ \"$(cut -d ' ' -f 2 " + shellQuote(writerOutcomeProofPath) + ")\" != 0 ] && " +
			"[ -d " + shellQuote(root) + " ] && [ ! -L " + shellQuote(root) + " ]; then "
		for _, file := range []struct {
			source, target string
			limit          int
		}{
			{path.Join(workspace, export.EvidenceDescriptorPath), failureDescriptorProof, failureDescriptorBytes},
			{path.Join(workspace, source.Path), failureTranscriptProof, MaxFailureTranscriptBytes},
		} {
			script += "if [ -f " + shellQuote(file.source) + " ] && [ ! -L " + shellQuote(file.source) +
				" ]; then head -c " + strconv.Itoa(file.limit+1) + " " + shellQuote(file.source) +
				" > " + shellQuote(file.target) + "; fi; "
		}
		script += "fi; "
	}
	return script + "sync"
}

func readFailureTranscript(archive io.ReadSeeker, expected export.EvidenceSource) ([]byte, bool, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	descriptor, found, err := extractArchiveRegularFile(archive, failureDescriptorProof, failureDescriptorBytes)
	if err != nil && !errors.Is(err, errArchiveRegularFileInvalid) {
		return nil, false, err
	}
	if err != nil || !found {
		return nil, false, nil
	}
	manifest, err := export.DecodeEvidenceSourceManifest(descriptor)
	if err != nil {
		return nil, false, nil
	}
	matches := 0
	for _, source := range manifest.Sources {
		if source.Label == expected.Label {
			if source != expected {
				return nil, false, nil
			}
			matches++
		}
	}
	if matches != 1 {
		return nil, false, nil
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	body, found, err := extractArchiveRegularFile(archive, failureTranscriptProof, MaxFailureTranscriptBytes)
	// stderr can interrupt stream-json. Preserve valid UTF-8 as plain text,
	// never assert that a failed CLI produced valid JSONL.
	if err != nil && !errors.Is(err, errArchiveRegularFileInvalid) {
		return nil, false, err
	}
	return body, err == nil && found && utf8.Valid(body), nil
}

func (b *Backend) failureEvidencePath(runID, nonce string) (string, error) {
	root := filepath.Join(b.cfg.ExportRoot, "failure-evidence")
	if err := prepareExportRoot(root); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("failure evidence root is not private")
	}
	return filepath.Join(root, runID+"-"+nonce+".txt"), nil
}

func (b *Backend) captureFailureEvidence(ctx context.Context, hs HandoffSpec, st *runState, archive io.ReadSeeker, status int) error {
	if hs.Agent.FailureTranscript == nil || b.cfg.Journal == nil {
		return nil
	}
	// Neither teardown nor terminal close may erase the only source while a
	// host write or journal amendment is incomplete.
	st.preserveForRecovery, st.leaveJournalOpen = true, true
	if err := b.cfg.Journal.MarkWriterFailed(ctx, hs.RunID, status); err != nil {
		return err
	}
	body, valid, err := readFailureTranscript(archive, *hs.Agent.FailureTranscript)
	if err != nil {
		return err
	}
	digest := ""
	if valid {
		if err := prepareExportRoot(b.cfg.ExportRoot); err != nil {
			return err
		}
		dir, err := os.MkdirTemp(b.cfg.ExportRoot, "failure-scan-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		if err := os.WriteFile(filepath.Join(dir, "transcript.txt"), body, 0o600); err != nil {
			return err
		}
		if err := b.cfg.Scanner.Scan(ctx, dir); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(err, ErrOutputScanRefused) {
				return errors.New("failure transcript scan could not complete (details withheld)")
			}
			valid = false // Scanner details may contain matched credentials.
		}
		if valid {
			file, err := b.failureEvidencePath(hs.RunID, st.ownershipLabel.Value)
			if err != nil {
				return err
			}
			if err := atomicfile.WriteFileNoReplace(file, body, 0o600); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return err
				}
				prior, readErr := readPrivateFailureFile(file)
				if readErr != nil || !bytes.Equal(prior, body) {
					return errors.New("failure transcript conflicts with retained bytes")
				}
			}
			digest = contentaddr.Sum(body)
		}
	}
	if err := b.cfg.Journal.MarkFailureEvidence(ctx, hs.RunID, digest, !valid); err != nil {
		return err
	}
	st.preserveForRecovery, st.leaveJournalOpen = false, false
	return nil
}

func readPrivateFailureFile(file string) ([]byte, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > MaxFailureTranscriptBytes {
		return nil, errors.New("invalid retained failure transcript file")
	}
	f, err := os.Open(file) //nolint:gosec // derived private path, never a returned path
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	return io.ReadAll(io.LimitReader(f, MaxFailureTranscriptBytes+1))
}

func (b *Backend) failedRecoveryResult(ctx context.Context, hs HandoffSpec, rec HandoffJournalRecord) (*RecoveryResult, error) {
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	if rec.RunID != hs.RunID || rec.WriterFailureStatus == nil {
		return nil, fmt.Errorf("%w: invalid failure record identity", ErrInvalidJournalRecord)
	}
	out := &RecoveryResult{Outcome: RecoveryFailed, FailureStatus: *rec.WriterFailureStatus}
	if hs.Agent.FailureTranscript == nil {
		return out, nil
	}
	out.FailureEvidence = &FailureEvidence{Unavailable: rec.FailureEvidenceUnavailable || rec.FailureEvidenceDigest == ""}
	if out.FailureEvidence.Unavailable {
		return out, nil
	}
	file, err := b.failureEvidencePath(rec.RunID, rec.OwnershipToken)
	if err != nil {
		return nil, err
	}
	body, err := readPrivateFailureFile(file)
	if err != nil {
		return nil, err
	}
	if contentaddr.Sum(body) != rec.FailureEvidenceDigest || !utf8.Valid(body) {
		return nil, errors.New("retained failure transcript failed integrity verification")
	}
	// Re-scan only these bytes, not other runs' private diagnostics.
	dir, err := os.MkdirTemp(b.cfg.ExportRoot, "failure-rescan-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "transcript.txt"), body, 0o600); err != nil {
		return nil, err
	}
	if err := b.cfg.Scanner.Scan(ctx, dir); err != nil {
		return nil, fmt.Errorf("failure transcript scan refused (details withheld)")
	}
	out.FailureEvidence = &FailureEvidence{Body: body, Digest: rec.FailureEvidenceDigest}
	return out, nil
}

func (b *Backend) refreshFailureRecord(ctx context.Context, prior HandoffJournalRecord) (HandoffJournalRecord, error) {
	rec, err := b.cfg.Journal.Get(ctx, prior.RunID)
	if err != nil {
		return rec, err
	}
	if err := rec.Validate(); err != nil {
		return rec, err
	}
	if rec.RunID != prior.RunID || rec.OwnershipToken != prior.OwnershipToken || rec.SpecDigest != prior.SpecDigest ||
		rec.WriterFailureStatus == nil || (prior.WriterFailureStatus != nil && *rec.WriterFailureStatus != *prior.WriterFailureStatus) {
		return rec, fmt.Errorf("%w: failure record changed identity", ErrInvalidJournalRecord)
	}
	return rec, nil
}
