package ward

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func TestFailureObserverUsesConfiguredWorkspaceAndRejectsLinks(t *testing.T) {
	for _, tc := range []struct {
		status int
		linked bool
	}{
		{status: 1}, {status: 1, linked: true}, {status: 0},
	} {
		t.Run(strconv.Itoa(tc.status)+"-linked-"+strconv.FormatBool(tc.linked), func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "custom-workspace")
			evidence := filepath.Join(workspace, export.EvidenceWorkspaceDir)
			if err := os.MkdirAll(evidence, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "marker")
			if err := os.WriteFile(marker, []byte("nonce "+strconv.Itoa(tc.status)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, hs := failedTranscriptFixture(t)
			hs.Agent.OutcomeMarkerPath = marker
			file := filepath.Join(workspace, hs.Agent.FailureTranscript.Path)
			if tc.linked {
				if err := os.Symlink(marker, file); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(file, []byte("diagnostic"), 0o600); err != nil {
				t.Fatal(err)
			}
			transcript := filepath.Join(root, "captured")
			script := strings.NewReplacer(writerOutcomeProofPath, filepath.Join(root, "outcome"), failureDescriptorProof, filepath.Join(root, "descriptor"), failureTranscriptProof, transcript).Replace(writerOutcomeObserverCommand(workspace, hs))
			cmd := exec.CommandContext(t.Context(), "sh")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("observer: %s, %v", out, err)
			}
			_, err := os.Stat(transcript)
			wantCapture := tc.status != 0 && !tc.linked
			if wantCapture && err != nil || !wantCapture && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("capture status=%d linked=%v: %v", tc.status, tc.linked, err)
			}
		})
	}
}

func TestFailureTranscriptRejectsDuplicateAndSymlinkArchiveEntries(t *testing.T) {
	_, _, hs := failedTranscriptFixture(t)
	descriptor, _ := json.Marshal(export.EvidenceSourceManifest{Version: export.EvidenceSourceVersion, Sources: []export.EvidenceSource{*hs.Agent.FailureTranscript}})
	for _, entry := range []tarEntry{
		{name: strings.TrimPrefix(failureTranscriptProof, "/"), typeflag: tar.TypeSymlink, linkname: "/outside"},
		{name: strings.TrimPrefix(failureDescriptorProof, "/"), typeflag: tar.TypeReg, body: descriptor},
	} {
		archive := buildTar(t, []tarEntry{{name: strings.TrimPrefix(failureDescriptorProof, "/"), typeflag: tar.TypeReg, body: descriptor}, entry})
		file, err := os.OpenInRoot(filepath.Dir(archive), filepath.Base(archive))
		if err != nil {
			t.Fatal(err)
		}
		_, valid, err := readFailureTranscript(file, *hs.Agent.FailureTranscript)
		_ = file.Close()
		if valid || err != nil {
			t.Fatalf("archive refusal: valid=%v, error=%v", valid, err)
		}
	}
}

func failedTranscriptFixture(t *testing.T) (*handoffFixture, *fakeJournal, HandoffSpec) {
	t.Helper()
	fx := newHandoffFixture(t)
	fx.cfg.ExportRoot = t.TempDir()
	j := fx.journalled()
	hs := testHandoffSpec()
	hs.Agent.OutcomeMarkerPath = "/workspace/.freeside-evidence/.control/writer-outcome"
	hs.Agent.Command = []string{"sh", "-c", "printf '%s 1\\n' " + WriterNoncePlaceholder + " > " + hs.Agent.OutcomeMarkerPath}
	hs.Agent.FailureTranscript = &export.EvidenceSource{
		Label: "agent-transcript", MediaType: "application/jsonl",
		Path: ".freeside-evidence/agent-transcript.jsonl", HeadBinding: export.EvidenceHeadIndependent,
		SensitivityClass: export.EvidenceSensitivitySensitive, ProducerInvocationID: "inv-failed",
	}
	fx.rt.writerStatus = 1
	fx.rt.failureTranscript = []byte("Provider refused the request.\n")
	var err error
	fx.rt.failureDescriptor, err = json.Marshal(export.EvidenceSourceManifest{Version: export.EvidenceSourceVersion, Sources: []export.EvidenceSource{*hs.Agent.FailureTranscript}})
	if err != nil {
		t.Fatal(err)
	}
	return fx, j, hs
}

func TestFailedWriterRetainsTranscriptWithoutSourceExport(t *testing.T) {
	fx, j, hs := failedTranscriptFixture(t)
	// The ordinary source exporter is deliberately unusable. Diagnostics must
	// not depend on whether partially written source forms a valid export.
	fx.rt.exportTarPath = "/does-not-exist"
	b := fx.backend(t)
	if _, err := b.Handoff(context.Background(), hs); !errors.Is(err, ErrWriterFailed) {
		t.Fatalf("handoff: %v", err)
	}
	rec := j.snapshot(hs.RunID)
	if rec.Outcome == nil || *rec.Outcome != HandoffFailed || rec.ExportDir != "" || rec.WriterComplete || rec.FailureEvidenceDigest != contentaddr.Sum(fx.rt.failureTranscript) {
		t.Fatalf("record: %+v", rec)
	}
	fx.assertReaped(t)
	for range 2 {
		out, err := b.Recover(context.Background(), hs.RunID, hs)
		if err != nil || out.Outcome != RecoveryFailed || out.ExportDir != "" || out.FailureEvidence == nil || !bytes.Equal(out.FailureEvidence.Body, fx.rt.failureTranscript) {
			t.Fatalf("recovery: %+v, %v", out, err)
		}
	}
}

func TestFailureTranscriptUpgradePreservesHistoricalHandoff(t *testing.T) {
	fx, _, hs := failedTranscriptFixture(t)
	legacy := hs
	legacy.Agent.FailureTranscript = nil
	b := fx.backend(t)
	if _, err := b.Handoff(context.Background(), legacy); !errors.Is(err, ErrWriterFailed) {
		t.Fatalf("legacy handoff: %v", err)
	}
	out, err := b.Recover(context.Background(), hs.RunID, hs)
	if err != nil || out.Outcome != RecoveryFailed || out.FailureEvidence != nil {
		t.Fatalf("upgrade must preserve the old capture omission: %+v, %v", out, err)
	}
	hs.Agent.Image = "changed-image@sha256:" + strings.Repeat("ab", 32)
	if _, err := b.Recover(context.Background(), hs.RunID, hs); !errors.Is(err, ErrInvalidJournalRecord) {
		t.Fatalf("upgrade accepted another spec change: %v", err)
	}
}

func TestFailureEvidenceAmendmentFailurePreservesWorkspace(t *testing.T) {
	fx, j, hs := failedTranscriptFixture(t)
	j.failFailureEvidence = errors.New("temporary journal failure")
	b := fx.backend(t)
	if _, err := b.Handoff(context.Background(), hs); !errors.Is(err, ErrWriterFailed) {
		t.Fatalf("handoff: %v", err)
	}
	rec := j.snapshot(hs.RunID)
	if rec.Outcome != nil || rec.WriterFailureStatus == nil || rec.FailureEvidenceDigest != "" {
		t.Fatalf("record: %+v", rec)
	}
	j.failFailureEvidence = nil
	out, err := b.Recover(context.Background(), hs.RunID, hs)
	if err != nil || out.FailureEvidence == nil || !bytes.Equal(out.FailureEvidence.Body, fx.rt.failureTranscript) {
		t.Fatalf("recovery: %+v, %v", out, err)
	}
	fx.assertReaped(t)
}

func TestFailureEvidenceScannerIOFailurePreservesWorkspace(t *testing.T) {
	fx, j, hs := failedTranscriptFixture(t)
	scanErr := error(os.ErrPermission)
	fx.cfg.Scanner = scannerFunc(func(context.Context, string) error { return scanErr })
	b := fx.backend(t)
	if _, err := b.Handoff(t.Context(), hs); !errors.Is(err, ErrWriterFailed) {
		t.Fatalf("handoff: %v", err)
	}
	rec := j.snapshot(hs.RunID)
	if rec.Outcome != nil || rec.WriterFailureStatus == nil || rec.FailureEvidenceUnavailable || rec.FailureEvidenceDigest != "" {
		t.Fatalf("scanner failure became permanent: %+v", rec)
	}
	scanErr = nil
	out, err := b.Recover(t.Context(), hs.RunID, hs)
	if err != nil || out.FailureEvidence == nil || !bytes.Equal(out.FailureEvidence.Body, fx.rt.failureTranscript) {
		t.Fatalf("scanner retry lost transcript: %+v, %v", out, err)
	}
	fx.assertReaped(t)
}

func TestFailedWriterObserverDeleteFailureKeepsFailedOutcome(t *testing.T) {
	fx, j, hs := failedTranscriptFixture(t)
	failed := false
	fx.rt.onDeleteContainer = func(id string) (bool, error) {
		if id == namesFor(hs.RunID).WriterObserver {
			failed = true
			return false, errors.New("temporary observer deletion failure")
		}
		return false, nil
	}
	b := fx.backend(t)
	if _, err := b.Handoff(context.Background(), hs); !errors.Is(err, ErrWriterFailed) {
		t.Fatalf("handoff: %v", err)
	}
	if !failed {
		t.Fatal("delete failure was not exercised")
	}
	fx.rt.onDeleteContainer = nil
	if rec := j.snapshot(hs.RunID); rec.WriterFailureStatus == nil || *rec.WriterFailureStatus != 1 {
		t.Fatalf("failure status lost: %+v", rec)
	}
	for range 2 {
		out, err := b.Recover(context.Background(), hs.RunID, hs)
		if err != nil || out.Outcome != RecoveryFailed || out.FailureEvidence == nil || !bytes.Equal(out.FailureEvidence.Body, fx.rt.failureTranscript) {
			t.Fatalf("recovery: %+v, %v", out, err)
		}
	}
	fx.assertReaped(t)
}

type failedArchiveSeek struct{ *bytes.Reader }

func (failedArchiveSeek) Seek(int64, int) (int64, error) { return 0, os.ErrPermission }

func TestFailureTranscriptArchiveIOErrorRemainsRetryable(t *testing.T) {
	_, _, hs := failedTranscriptFixture(t)
	_, valid, err := readFailureTranscript(failedArchiveSeek{bytes.NewReader(nil)}, *hs.Agent.FailureTranscript)
	if valid || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("archive I/O was treated as absent: %v, %v", valid, err)
	}
}

func TestFailureEvidenceRefusalKeepsFailedOutcome(t *testing.T) {
	for _, kind := range []string{"descriptor", "oversized", "invalid utf8", "scanner", "foreign invocation"} {
		t.Run(kind, func(t *testing.T) {
			fx, j, hs := failedTranscriptFixture(t)
			switch kind {
			case "descriptor":
				fx.rt.failureDescriptor = []byte("invalid")
			case "oversized":
				fx.rt.failureTranscript = bytes.Repeat([]byte("x"), MaxFailureTranscriptBytes+1)
			case "invalid utf8":
				fx.rt.failureTranscript = []byte{0xff}
			case "scanner":
				fx.cfg.Scanner = scannerFunc(func(context.Context, string) error {
					return errors.Join(ErrOutputScanRefused, errors.New("SECRET-MUST-NOT-ESCAPE"))
				})
			case "foreign invocation":
				fx.rt.failureDescriptor = bytes.ReplaceAll(fx.rt.failureDescriptor, []byte("inv-failed"), []byte("inv-foreign"))
			}
			b := fx.backend(t)
			_, err := b.Handoff(context.Background(), hs)
			if !errors.Is(err, ErrWriterFailed) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("handoff: %v", err)
			}
			rec := j.snapshot(hs.RunID)
			if !rec.FailureEvidenceUnavailable || rec.FailureEvidenceDigest != "" || rec.Outcome == nil || *rec.Outcome != HandoffFailed {
				t.Fatalf("record: %+v", rec)
			}
			fx.assertReaped(t)
		})
	}
}

func TestFailureEvidenceReplayRejectsTamperedPrivateFile(t *testing.T) {
	fx, j, hs := failedTranscriptFixture(t)
	b := fx.backend(t)
	if _, err := b.Handoff(context.Background(), hs); !errors.Is(err, ErrWriterFailed) {
		t.Fatal(err)
	}
	rec := j.snapshot(hs.RunID)
	file, err := b.failureEvidencePath(rec.RunID, rec.OwnershipToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("replaced"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Recover(context.Background(), hs.RunID, hs); err == nil {
		t.Fatal("accepted tampered failure evidence")
	}
}
