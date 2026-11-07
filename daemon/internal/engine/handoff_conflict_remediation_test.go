package engine

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// An ordinary round's input keeps the bytes, and so the digest, it had before
// the handoff conflict member existed and Adjudication became a pointer.
// legacyRemediationInput is that earlier shape, member for member.
func TestOrdinaryRemediationInputKeepsItsBytes(t *testing.T) {
	t.Parallel()
	type legacyRemediationInput struct {
		Version              string                     `json:"version"`
		RunID                domain.RunID               `json:"run_id"`
		Round                int                        `json:"round"`
		BaseSHA              string                     `json:"base_sha"`
		HeadSHA              string                     `json:"head_sha"`
		Instruction          string                     `json:"instruction"`
		CandidatePatchBase64 []byte                     `json:"candidate_patch_base64"`
		Adjudication         domain.FindingAdjudication `json:"adjudication"`
		Findings             []domain.Finding           `json:"findings"`
		ExternalFindings     []quotedExternalFinding    `json:"external_findings,omitempty"`
		DriftAuditDigest     domain.Digest              `json:"drift_audit_digest,omitempty"`
		Reversals            []remediationReversal      `json:"reversals,omitempty"`
	}
	adjudication := domain.FindingAdjudication{
		RunID: "run-1", Round: 2, Revision: 1,
		Entries: []domain.FindingAdjudicationEntry{{FindingID: "finding-1"}},
	}
	findings := []domain.Finding{{ID: "finding-1", RunID: "run-1", Message: "a finding"}}
	legacy, err := json.Marshal(legacyRemediationInput{
		Version: remediationInputVersion, RunID: "run-1", Round: 2,
		BaseSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("c", 40),
		Instruction: remediationInstruction, CandidatePatchBase64: []byte("patch"),
		Adjudication: adjudication, Findings: findings,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := json.Marshal(remediationInput{
		Version: remediationInputVersion, RunID: "run-1", Round: 2,
		BaseSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("c", 40),
		Instruction: remediationInstruction, CandidatePatchBase64: []byte("patch"),
		Adjudication: &adjudication, Findings: findings,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, legacy) {
		t.Fatalf("ordinary input = %s, want %s", current, legacy)
	}
}

func handoffConflictAuthority(cause domain.HandoffConflictCause) domain.PublicationSuccessor {
	return domain.PublicationSuccessor{
		Version: domain.PublicationHandoffConflictVersion, Origin: domain.PublicationSuccessorHandoffConflict,
		RunID: "run-1", PredecessorItemID: "production-ready-run-1",
		PriorReviewInvocationID: "review-2", ReviewRound: 3,
		Reentry:              &domain.PublicationSuccessorReentry{BaseSHA: "tip-sha", HeadSHA: "head-sha"},
		HandoffConflictCause: cause, FirstPublication: true,
	}
}

// The input's account is the sealed authority's: its tip and cause, and only
// the members that cause carries, each within its bound.
func TestHandoffConflictInputValidFor(t *testing.T) {
	t.Parallel()
	textual := handoffConflictAuthority(domain.HandoffConflictTextual)
	failed := handoffConflictAuthority(domain.HandoffConflictMergeVerificationFailed)
	paths := handoffConflictInput{
		TipSHA: "tip-sha", Cause: domain.HandoffConflictTextual,
		ConflictedPaths: []string{`"a.go"`}, PathsTruncated: true,
	}
	verification := handoffConflictInput{
		TipSHA: "tip-sha", Cause: domain.HandoffConflictMergeVerificationFailed,
		ReportDigest:           domain.Digest("sha256:" + strings.Repeat("a", 64)),
		VerificationSteps:      []verify.Step{{Argv: []string{"make", "test"}, ExitCode: 2}},
		TranscriptDigest:       domain.Digest("sha256:" + strings.Repeat("b", 64)),
		VerificationTranscript: "FAIL", TranscriptTruncated: true,
	}
	if !paths.validFor(textual) || !verification.validFor(failed) {
		t.Fatal("a handoff conflict's own account was refused")
	}
	if paths.validFor(failed) || verification.validFor(textual) {
		t.Fatal("an account was admitted under the other cause")
	}
	for name, input := range map[string]handoffConflictInput{
		"other tip": func() handoffConflictInput { i := paths; i.TipSHA = "evidence-base"; return i }(),
		"a report":  func() handoffConflictInput { i := paths; i.ReportDigest = verification.ReportDigest; return i }(),
		"a transcript": func() handoffConflictInput {
			i := paths
			i.VerificationTranscript = "output"
			return i
		}(),
		"too many paths": func() handoffConflictInput {
			i := paths
			i.ConflictedPaths = make([]string, handoffConflictPathLimit+1)
			return i
		}(),
	} {
		if input.validFor(textual) {
			t.Errorf("%s: textual account admitted", name)
		}
	}
	for name, input := range map[string]handoffConflictInput{
		"a path": func() handoffConflictInput {
			i := verification
			i.ConflictedPaths = []string{`"a.go"`}
			return i
		}(),
		"truncated paths": func() handoffConflictInput { i := verification; i.PathsTruncated = true; return i }(),
		"no report":       func() handoffConflictInput { i := verification; i.ReportDigest = ""; return i }(),
		"long transcript": func() handoffConflictInput {
			i := verification
			i.VerificationTranscript = strings.Repeat("x", handoffConflictTranscriptBytes+1)
			return i
		}(),
	} {
		if input.validFor(failed) {
			t.Errorf("%s: verification account admitted", name)
		}
	}
	unsealed := textual
	unsealed.Reentry = nil
	if paths.validFor(unsealed) {
		t.Fatal("an account was admitted without a sealed tip")
	}
}

// A handoff check sealed before migration 0129 names no workflow audit, so
// the store gate refuses its tip as a remediation base; the conflict blocks
// on today's item instead of failing the seal. The check runs before any
// store read, so no store is needed.
func TestHandoffConflictOnAnUnboundCheckBlocks(t *testing.T) {
	t.Parallel()
	w := &productionPublicationWorkflow{}
	task := productionPublicationTask{RunID: "run-1", PublicationID: "publish-run-1", HeadSHA: "head-1"}
	check := domain.HandoffCheck{
		RunID: task.RunID, PublicationID: task.PublicationID, HeadSHA: task.HeadSHA,
		EvidenceBaseSHA: "base-1", TipSHA: "base-2",
	}
	for _, cause := range domain.AllHandoffConflictCauses {
		_, _, ok, err := w.handoffConflictSuccessor(t.Context(), task, check, cause)
		if ok || err != nil {
			t.Fatalf("%s: remediates = %v, err = %v; want a block", cause, ok, err)
		}
	}
}
