package signet_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// stubPrompts answers InvocationPrompt from fixed values.
type stubPrompts struct {
	prompt string
	found  bool
	err    error
}

func (s stubPrompts) InvocationPrompt(context.Context, domain.InvocationID) (string, bool, error) {
	return s.prompt, s.found, s.err
}

// transcriptFixture is a corpus fixture with a blob store and a prompt
// reader the test can swap between requests.
type transcriptFixture struct {
	corpusFixture
	blobs   *signet.BlobStore
	prompts *signet.InvocationPromptReader
	handler http.Handler
}

func newTranscriptFixture(t *testing.T) transcriptFixture {
	t.Helper()
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var reader signet.InvocationPromptReader
	f := newCorpusFixture(t, signet.WithBlobStore(blobs),
		signet.WithInvocationPrompts(func() signet.InvocationPromptReader { return reader }))
	f.seedAuthIdentity(t)
	return transcriptFixture{
		corpusFixture: f, blobs: blobs, prompts: &reader,
		handler: signet.NewHTTPHandler(f.service, testAuthorizer),
	}
}

// seedWriter records a writer invocation admitted for runID, with claims
// when any are given.
func (f transcriptFixture) seedWriter(t *testing.T, runID domain.RunID, invocation domain.InvocationID, claims ...domain.AgentClaim) {
	t.Helper()
	attempt := corpusAttempt(runID, invocation)
	agentInvocation, err := domain.NewAgentInvocation(
		invocation, []domain.ArtifactID{domain.ArtifactID("spec-" + invocation)}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.PutRun(context.Background(), corpusRun(runID, attempt))
	})
	f.seedAdmission(t, runID, corpusStageID(runID), attempt.ID, invocation)
	f.mustWrite(t, func(tx *store.WriteTx) error {
		// The claim set's foreign key needs the invocation row.
		if err := tx.PutAgentInvocation(context.Background(), agentInvocation); err != nil {
			return err
		}
		if len(claims) == 0 {
			return nil
		}
		return tx.PutAgentClaims(context.Background(), invocation, claims)
	})
}

// storeBytes puts content in the blob store and returns its digest.
func (f transcriptFixture) storeBytes(t *testing.T, content []byte) domain.Digest {
	t.Helper()
	sum := sha256.Sum256(content)
	digest := domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
	if _, err := f.blobs.Put(digest, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	return digest
}

func (f transcriptFixture) get(t *testing.T, runID domain.RunID, invocation domain.InvocationID) (int, []byte) {
	t.Helper()
	response := authenticatedRequest(t, f.handler, http.MethodGet,
		"/runs/"+string(runID)+"/invocations/"+string(invocation)+"/transcript", nil)
	return response.Code, response.Body.Bytes()
}

func (f transcriptFixture) mustGet(t *testing.T, runID domain.RunID, invocation domain.InvocationID) signet.InvocationTranscript {
	t.Helper()
	code, body := f.get(t, runID, invocation)
	if code != http.StatusOK {
		t.Fatalf("GET transcript %s = %d %s", invocation, code, body)
	}
	var out signet.InvocationTranscript
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func transcriptClaimFixture(
	invocation domain.InvocationID, artifact domain.ArtifactID, digest domain.Digest, mediaType domain.EvidenceMediaType,
) domain.AgentClaim {
	return domain.AgentClaim{
		Label: "agent-transcript", Artifact: artifact, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocation,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivitySensitive,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: mediaType, SizeBytes: 12, CreatedAt: transcriptFixtureTime,
			Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
}

var transcriptFixtureTime = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

// The read serves a successful writer's transcript claim and a failed
// writer's retained one, each available while its bytes are held, beside
// the prompt the driver reads back.
func TestInvocationTranscriptServesTheInvocationsClaim(t *testing.T) {
	t.Parallel()
	f := newTranscriptFixture(t)
	*f.prompts = stubPrompts{prompt: "Implement the approved work item.", found: true}

	succeeded := domain.InvocationID("writer-succeeded")
	digest := f.storeBytes(t, []byte(`{"type":"result"}`+"\n"))
	f.seedWriter(t, "run-transcript", succeeded,
		transcriptClaimFixture(succeeded, "agent-transcript-writer-succeeded", digest, domain.EvidenceMediaApplicationJSONL))
	got := f.mustGet(t, "run-transcript", succeeded)
	if got.RunID != "run-transcript" || got.InvocationID != succeeded ||
		got.Availability != signet.InvocationTranscriptAvailable || got.Transcript == nil ||
		got.Transcript.Digest != digest || got.Transcript.Metadata.Availability != domain.EvidenceAvailable ||
		got.Prompt == nil || *got.Prompt != "Implement the approved work item." {
		t.Fatalf("succeeded transcript = %#v", got)
	}

	failed := domain.InvocationID("writer-failed")
	failedDigest := f.storeBytes(t, []byte("the writer's stdout\n"))
	f.seedWriter(t, "run-transcript-failed", failed,
		transcriptClaimFixture(failed, "artifact-failed-transcript-writer-failed", failedDigest, domain.EvidenceMediaTextPlain))
	got = f.mustGet(t, "run-transcript-failed", failed)
	if got.Availability != signet.InvocationTranscriptAvailable || got.Transcript == nil ||
		got.Transcript.Artifact != "artifact-failed-transcript-writer-failed" {
		t.Fatalf("failed transcript = %#v", got)
	}
}

// An invocation with no claim, or a claim whose bytes are gone, reads as
// unavailable with no claim.
func TestInvocationTranscriptUnavailable(t *testing.T) {
	t.Parallel()
	f := newTranscriptFixture(t)

	f.seedWriter(t, "run-no-claim", "writer-no-claim")
	got := f.mustGet(t, "run-no-claim", "writer-no-claim")
	if got.Availability != signet.InvocationTranscriptUnavailable || got.Transcript != nil {
		t.Fatalf("no claim = %#v", got)
	}

	absent := domain.Digest("sha256:" + string(bytes.Repeat([]byte("c"), 64)))
	f.seedWriter(t, "run-bytes-gone", "writer-bytes-gone",
		transcriptClaimFixture("writer-bytes-gone", "agent-transcript-writer-bytes-gone", absent, domain.EvidenceMediaApplicationJSONL))
	got = f.mustGet(t, "run-bytes-gone", "writer-bytes-gone")
	if got.Availability != signet.InvocationTranscriptUnavailable || got.Transcript != nil {
		t.Fatalf("bytes gone = %#v", got)
	}
}

// The admission binds the invocation to its run: an unknown run, an unknown
// invocation, and an invocation admitted for another run are all not found.
func TestInvocationTranscriptNotFound(t *testing.T) {
	t.Parallel()
	f := newTranscriptFixture(t)
	f.seedWriter(t, "run-mine", "writer-mine")
	f.seedWriter(t, "run-other", "writer-other")
	for _, tc := range []struct {
		name       string
		run        domain.RunID
		invocation domain.InvocationID
	}{
		{"unknown run", "run-missing", "writer-mine"},
		{"unknown invocation", "run-mine", "writer-missing"},
		{"another run's invocation", "run-mine", "writer-other"},
	} {
		if code, body := f.get(t, tc.run, tc.invocation); code != http.StatusNotFound {
			t.Errorf("%s = %d %s, want 404", tc.name, code, body)
		}
	}
}

// A claim set that disagrees with the invocation fails the read with no
// content: a transcript naming another producer, and two transcripts.
func TestInvocationTranscriptRefusesADisagreeingClaimSet(t *testing.T) {
	t.Parallel()
	f := newTranscriptFixture(t)
	digest := f.storeBytes(t, []byte("stream\n"))
	f.seedWriter(t, "run-foreign-claim", "writer-foreign-claim",
		transcriptClaimFixture("writer-elsewhere", "agent-transcript-elsewhere", digest, domain.EvidenceMediaApplicationJSONL))
	twice := domain.InvocationID("writer-twice")
	f.seedWriter(t, "run-twice", twice,
		transcriptClaimFixture(twice, "agent-transcript-writer-twice", digest, domain.EvidenceMediaApplicationJSONL),
		transcriptClaimFixture(twice, "artifact-failed-transcript-writer-twice", digest, domain.EvidenceMediaTextPlain))
	for run, invocation := range map[domain.RunID]domain.InvocationID{
		"run-foreign-claim": "writer-foreign-claim", "run-twice": twice,
	} {
		code, body := f.get(t, run, invocation)
		if code != http.StatusInternalServerError || bytes.Contains(body, []byte(digest)) {
			t.Errorf("%s = %d %s, want 500 with no content", invocation, code, body)
		}
	}
}

// The prompt is null with no reader, a not-found answer, or a reader error;
// the error leaves the transcript readable.
func TestInvocationTranscriptPromptIsNullUnlessServed(t *testing.T) {
	t.Parallel()
	f := newTranscriptFixture(t)
	digest := f.storeBytes(t, []byte("stream\n"))
	f.seedWriter(t, "run-prompt", "writer-prompt",
		transcriptClaimFixture("writer-prompt", "agent-transcript-writer-prompt", digest, domain.EvidenceMediaApplicationJSONL))
	for _, tc := range []struct {
		name   string
		reader signet.InvocationPromptReader
	}{
		{"no reader", nil},
		{"not found", stubPrompts{}},
		{"refused", stubPrompts{err: errors.New("intent prompt disagrees with admitted inputs")}},
	} {
		*f.prompts = tc.reader
		got := f.mustGet(t, "run-prompt", "writer-prompt")
		if got.Prompt != nil || got.Availability != signet.InvocationTranscriptAvailable {
			t.Errorf("%s = %#v, want a null prompt beside the transcript", tc.name, got)
		}
	}
}
