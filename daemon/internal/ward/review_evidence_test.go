package ward

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func TestDecodeRetainedCodexReviewPreservesInvalidUTF8(t *testing.T) {
	events := []byte{'e', 0xff, 0x00}
	outcome := codexGoldenReviewOutcome(string(events))
	request := exec.ReviewRequest{
		Admission: testReviewAdmission("run-golden"),
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
	got, err := DecodeRetainedCodexReview(binding, requestBody, contentaddr.Sum(requestBody), "ready", body, contentaddr.Sum(append([]byte("ready\x00"), body...)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Collection.Events, append(events, '\n')) || collectionEvidence(codexReviewProvider{}, *got.Collection) != outcome.CollectionEvidence {
		t.Fatal("retained bytes no longer match their authenticated collection evidence")
	}
}

func TestDecodeRetainedCodexReview(t *testing.T) {
	for _, corruption := range []string{"none", "request before admission", "request admission", "request legacy", "request run", "request round", "request head", "request digest", "outcome digest", "outcome invocation", "collection", "provider", "state"} {
		t.Run(corruption, func(t *testing.T) {
			outcome := codexGoldenReviewOutcome("{}")
			request := exec.ReviewRequest{
				Admission: testReviewAdmission("run-golden"),
				RunID:     "run-golden", Round: 1, Repo: "owner/repo", RepositoryID: 1, BaseRef: "main",
				BaseSHA: outcome.Result.BaseSHA, HeadSHA: outcome.Result.HeadSHA, Workspace: "/workspace",
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
				outcome.Result.Provider = "anthropic"
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
			if corruption == "request before admission" {
				// A round reviewed before requests carried an admission: its
				// retained evidence still reads.
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
			got, err := DecodeRetainedCodexReview(binding, requestBody, requestDigest, state, body, bodyDigest)
			if corruption == "none" || corruption == "request before admission" {
				if err != nil {
					t.Fatal(err)
				}
				if got.Collection == nil || got.CollectionEvidence != outcome.CollectionEvidence {
					t.Fatal("retained collection lost")
				}
			} else if err == nil {
				t.Fatal("corrupt retained evidence accepted")
			}
		})
	}
}
