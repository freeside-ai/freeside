package ward

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func TestDecodeRetainedCodexReviewPreservesInvalidUTF8(t *testing.T) {
	t.Parallel()
	events := []byte{'e', 0xff, 0x00}
	outcome := codexGoldenReviewOutcome(string(events))
	request := exec.ReviewRequest{
		Admission: testRecordedReviewAdmission("run-golden"),
		RunID:     "run-golden", Round: 1, Repo: "owner/repo", RepositoryID: 1, BaseRef: "main",
		BaseSHA: outcome.Result.BaseSHA, HeadSHA: outcome.Result.HeadSHA, Workspace: "/workspace",
		Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
	}
	requestBody, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.ReviewRequestRecord{InvocationID: outcome.InvocationID, RunID: request.RunID, Round: 1, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA}
	journalBody, journalDigest := testReviewJournalBinding(t, codexReviewTopologyVersion, outcome.InvocationID)
	got, err := DecodeRetainedCodexReview(binding, requestBody, contentaddr.Sum(requestBody), "ready", body, contentaddr.Sum(append([]byte("ready\x00"), body...)), journalBody, journalDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Collection.Events, append(events, '\n')) || collectionEvidence(codexReviewProvider{}, *got.Collection) != outcome.CollectionEvidence {
		t.Fatal("retained bytes no longer match their authenticated collection evidence")
	}
}

// TestDecodeRetainedCodexReview covers the retained-evidence read for both
// providers. The provider comes from the round's journal binding, so a
// Claude-routed round reads back under the Claude gate, and a binding that
// names the other provider, an unknown one, or another invocation, or a
// missing binding under evidence, is refused. A bare failure fence carries no
// evidence and reads without a binding.
func TestDecodeRetainedCodexReview(t *testing.T) {
	t.Parallel()
	reads := map[string]bool{
		"none": true, "request before admission": true, "claude": true,
		"codex legacy topology": true, "fence without binding": true,
	}
	for _, corruption := range []string{
		"none", "request before admission", "result before admission", "request admission", "request legacy",
		"request run", "request round", "request head", "request digest", "outcome digest", "outcome invocation",
		"collection", "provider", "returned admission", "state",
		"claude", "codex legacy topology", "claude binding over codex evidence", "codex binding over claude evidence",
		"binding unknown topology", "binding digest", "binding run", "binding missing", "fence without binding",
		"failure evidence without binding",
	} {
		t.Run(corruption, func(t *testing.T) {
			outcome := codexGoldenReviewOutcome("{}")
			admission := testRecordedReviewAdmission("run-golden")
			topology := codexReviewTopologyVersion
			switch corruption {
			case "claude", "codex binding over claude evidence":
				outcome = claudeGoldenReviewOutcome(`{"type":"result","is_error":false,"result":"reviewed"}`)
				admission = testClaudeReviewAdmission("run-golden")
				if corruption == "claude" {
					topology = claudeReviewTopologyVersion
				}
			case "codex legacy topology":
				topology = codexReviewTopologyVersionV2
			case "claude binding over codex evidence":
				topology = claudeReviewTopologyVersion
			case "binding unknown topology":
				topology = "unknown_review_read_only_v1"
			case "failure evidence without binding":
				// A failure that keeps its collection still carries evidence,
				// so it needs a binding to name its gate.
				outcome = codexGoldenReviewOutcome(`{"type":"turn.failed","error":{"message":"configuration invalid"}}`)
				if outcome.Result != nil || outcome.CollectionEvidence == "" {
					t.Fatalf("fixture is not a failure carrying evidence: %+v", outcome)
				}
			case "fence without binding":
				// The source writes a fence before any launch, so the round
				// may have no binding; with no evidence there is nothing to
				// hold to a provider.
				outcome = CodexReviewSourceOutcome{
					InvocationID: outcome.InvocationID, FailureClass: domain.ReviewFailureTransient, Failure: "review stopped",
				}
			}
			bindingRunID := outcome.InvocationID
			if corruption == "binding run" {
				bindingRunID = "review-foreign-1"
			}
			journalBody, journalDigest := testReviewJournalBinding(t, topology, bindingRunID)
			switch corruption {
			case "binding digest":
				journalDigest = contentaddr.Sum([]byte("another binding"))
			case "binding missing", "fence without binding", "failure evidence without binding":
				journalBody, journalDigest = nil, ""
			}
			request := exec.ReviewRequest{
				Admission: admission,
				RunID:     "run-golden", Round: 1, Repo: "owner/repo", RepositoryID: 1, BaseRef: "main",
				BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/workspace",
				Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
			}
			binding := domain.ReviewRequestRecord{InvocationID: outcome.InvocationID, RunID: request.RunID, Round: 1, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, RequestedAt: request.RequestedAt}
			state := "ready"
			switch corruption {
			case "request legacy":
				request.Instructions = exec.ReviewInstructionBinding{}
			case "request run":
				request.RunID = "foreign"
			case "request round":
				request.Round = 2
			case "request head":
				request.HeadSHA = "foreign"
			case "outcome invocation":
				outcome.InvocationID = "foreign"
			case "collection":
				outcome.Collection.Events = []byte("tampered")
			case "provider":
				// Mixed tags: Claude result evidence over a Codex collection
				// passes neither gate, so the Codex binding refuses it.
				outcome.Result.CompletionEvidence, _ = reviewResultEvidence(
					claudeReviewProvider{}, *outcome.Result, outcome.CollectionEvidence)
			case "returned admission":
				// A row that names another agent and recomputes its unkeyed
				// evidence is still held against the request it answers.
				outcome.Result.Admission.LineageGroup = "another-lineage"
				outcome.Result.CompletionEvidence, _ = reviewResultEvidence(
					codexReviewProvider{}, *outcome.Result, outcome.CollectionEvidence)
			case "state":
				state = "unknown"
			}
			if corruption == "request admission" {
				request.Admission.Role = domain.RoleImplementer
			}
			requestBody, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "request before admission" || corruption == "result before admission" {
				// A round requested before requests carried an admission
				// still reads when it failed, because a failure returns no
				// snapshot to hold against the request. A result always
				// returns one, so it cannot answer such a request.
				if corruption == "request before admission" {
					outcome = codexGoldenReviewOutcome(`{"type":"turn.failed","error":{"message":"configuration invalid"}}`)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(requestBody, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "admission")
				if requestBody, err = json.Marshal(fields); err != nil {
					t.Fatal(err)
				}
			}
			body, err := json.Marshal(outcome)
			if err != nil {
				t.Fatal(err)
			}
			requestDigest := contentaddr.Sum(requestBody)
			bodyDigest := contentaddr.Sum(append([]byte(state+"\x00"), body...))
			if corruption == "request digest" {
				requestDigest = "wrong"
			}
			if corruption == "outcome digest" {
				bodyDigest = "wrong"
			}
			got, err := DecodeRetainedCodexReview(binding, requestBody, requestDigest, state, body, bodyDigest, journalBody, journalDigest)
			if !reads[corruption] {
				if err == nil {
					t.Fatal("corrupt retained evidence accepted")
				}
				if strings.Contains(corruption, "binding") {
					if !errors.Is(err, domain.ErrInvalidReviewCompletionEvidence) {
						t.Fatalf("refusal = %v, want %v", err, domain.ErrInvalidReviewCompletionEvidence)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (got.Collection == nil) != (outcome.Collection == nil) || got.CollectionEvidence != outcome.CollectionEvidence {
				t.Fatal("retained collection lost")
			}
		})
	}
}

// claudeGoldenReviewOutcome is codexGoldenReviewOutcome for a Claude-routed
// round: the same fixed collection normalized through the real write path
// under the Claude provider, so its evidence carries the Claude version tags.
func claudeGoldenReviewOutcome(events string) CodexReviewSourceOutcome {
	source := newEquivalenceReviewSource()
	source.cfg.provider = claudeReviewProvider{}
	req := exec.ReviewRequest{
		Admission:    testClaudeReviewAdmission("run-golden"),
		RunID:        "run-golden",
		BaseSHA:      strings.Repeat("a", 40),
		HeadSHA:      strings.Repeat("b", 40),
		Instructions: testReviewInstructionBinding(),
	}
	return source.normalizeCollection(
		domain.InvocationID("review-golden-1"), req, codexGoldenReviewCollection(events),
	)
}

// testReviewJournalBinding returns a journal binding row and its digest. A
// retained read takes only the topology version and run ID from it.
func testReviewJournalBinding(t *testing.T, topologyVersion string, runID domain.InvocationID) ([]byte, string) {
	t.Helper()
	body, err := json.Marshal(CodexReviewJournalBinding{TopologyVersion: topologyVersion, RunID: string(runID)})
	if err != nil {
		t.Fatal(err)
	}
	return body, contentaddr.Sum(body)
}

// TestDecodeRetainedCodexReviewRequestFacts covers the journal a pre-0067
// in-flight review leaves behind: the decoder recovers its request time only
// for the exact candidate, and a request from before instruction binding still
// reads, as does one naming the prospective merge a pre-#2660 re-entry
// reviewed. TestProductionReviewUpgradePreservesRequestTime runs the upgrade
// end to end on an intact and a damaged journal.
func TestDecodeRetainedCodexReviewRequestFacts(t *testing.T) {
	t.Parallel()
	for _, corruption := range []string{"none", "legacy", "evaluated", "run", "round", "base", "head", "time", "digest"} {
		t.Run(corruption, func(t *testing.T) {
			t.Parallel()
			request := exec.ReviewRequest{
				Admission: testRecordedReviewAdmission("run-golden"),
				RunID:     "run-golden", Round: 1, Repo: "owner/repo", RepositoryID: 1, BaseRef: "main",
				BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/workspace",
				Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
			}
			binding := domain.ReviewRequestRecord{
				InvocationID: "inv-review", RunID: request.RunID, Round: request.Round,
				BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA,
			}
			switch corruption {
			case "evaluated":
				request.LegacyEvaluatedSHA = strings.Repeat("c", 40)
			case "run":
				request.RunID = "other-run"
			case "round":
				request.Round++
			case "base":
				request.BaseSHA = "other-base"
			case "head":
				request.HeadSHA = "other-head"
			case "time":
				request.RequestedAt = time.Time{}
			}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "legacy" {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "instructions")
				if body, err = json.Marshal(fields); err != nil {
					t.Fatal(err)
				}
			}
			digest := contentaddr.Sum(body)
			if corruption == "digest" {
				digest = "wrong"
			}
			got, err := DecodeRetainedCodexReviewRequestFacts(binding, body, digest)
			switch corruption {
			case "none", "legacy", "evaluated":
			case "time":
				if err == nil {
					t.Fatalf("journal without a request time decoded: %+v", got)
				}
				return
			case "digest":
				if !errors.Is(err, domain.ErrInvalidReviewCompletionEvidence) {
					t.Fatalf("damaged journal = %v, want %v", err, domain.ErrInvalidReviewCompletionEvidence)
				}
				return
			default:
				if !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("journal for another candidate = %v, want %v", err, domain.ErrParentKeyMismatch)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.RequestedAt.Equal(codexReviewEpoch) {
				t.Fatalf("requested at = %s, want %s", got.RequestedAt, codexReviewEpoch)
			}
			got.RequestedAt = time.Time{}
			if got != binding {
				t.Fatalf("facts = %+v, want %+v", got, binding)
			}
		})
	}
}
