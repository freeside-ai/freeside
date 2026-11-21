package signet_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
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

// stubLive answers LiveTranscript from fixed values and records each ask.
type stubLive struct {
	availability signet.InvocationTranscriptAvailability
	live         *signet.LiveTranscript
	err          error
	mu           sync.Mutex
	asks         []string
}

func (s *stubLive) LiveTranscript(_ context.Context, id domain.InvocationID, after int64) (
	signet.InvocationTranscriptAvailability, *signet.LiveTranscript, error,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asks = append(s.asks, fmt.Sprintf("%s@%d", id, after))
	return s.availability, s.live, s.err
}

func (s *stubLive) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asks)
}

// transcriptFixture is a corpus fixture with a blob store, a prompt reader,
// and a live reader the test can swap between requests.
type transcriptFixture struct {
	corpusFixture
	blobs   *signet.BlobStore
	prompts *signet.InvocationPromptReader
	live    *signet.LiveTranscriptReader
	handler http.Handler
}

func newTranscriptFixture(t *testing.T) transcriptFixture {
	t.Helper()
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var reader signet.InvocationPromptReader
	var live signet.LiveTranscriptReader
	f := newCorpusFixture(t, signet.WithBlobStore(blobs),
		signet.WithInvocationPrompts(func() signet.InvocationPromptReader { return reader }),
		signet.WithLiveTranscripts(func() signet.LiveTranscriptReader { return live }))
	f.seedAuthIdentity(t)
	return transcriptFixture{
		corpusFixture: f, blobs: blobs, prompts: &reader, live: &live,
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
	return f.getQuery(t, runID, invocation, "")
}

func (f transcriptFixture) getQuery(t *testing.T, runID domain.RunID, invocation domain.InvocationID, query string) (int, []byte) {
	t.Helper()
	response := authenticatedRequest(t, f.handler, http.MethodGet,
		"/runs/"+string(runID)+"/invocations/"+string(invocation)+"/transcript"+query, nil)
	if response.Code == http.StatusOK && response.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}
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

// A running writer with no claim reads live: the reader is asked with the
// client's offset, and its records and counts reach the client. A withheld
// answer carries no records.
func TestInvocationTranscriptServesARunningWriter(t *testing.T) {
	t.Parallel()
	f := newTranscriptFixture(t)
	f.seedWriter(t, "run-live", "writer-live")
	live := &stubLive{
		availability: signet.InvocationTranscriptLive,
		live: &signet.LiveTranscript{
			Start: 8, End: 24, Records: []byte(`{"type":"user"}` + "\n"), OmittedBefore: true, UnreadableRecords: 2,
		},
	}
	*f.live = live
	code, body := f.getQuery(t, "run-live", "writer-live", "?after=8")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	var got signet.InvocationTranscript
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Availability != signet.InvocationTranscriptLive || got.Transcript != nil || got.Live == nil ||
		string(got.Live.Records) != `{"type":"user"}`+"\n" || got.Live.Start != 8 || got.Live.End != 24 ||
		!got.Live.OmittedBefore || got.Live.UnreadableRecords != 2 {
		t.Fatalf("live answer = %s", body)
	}
	if asks := live.asked(); !slices.Equal(asks, []string{"writer-live@8"}) {
		t.Fatalf("reader asked %q", asks)
	}

	// An empty window still serves records as an empty string, not null.
	*f.live = &stubLive{availability: signet.InvocationTranscriptLive, live: &signet.LiveTranscript{}}
	if _, body := f.get(t, "run-live", "writer-live"); !bytes.Contains(body, []byte(`"records":""`)) {
		t.Errorf("empty live answer = %s", body)
	}

	*f.live = &stubLive{availability: signet.InvocationTranscriptWithheld}
	got = f.mustGet(t, "run-live", "writer-live")
	if got.Availability != signet.InvocationTranscriptWithheld || got.Live != nil || got.Transcript != nil {
		t.Fatalf("withheld answer = %#v", got)
	}

	*f.live = &stubLive{availability: signet.InvocationTranscriptUnavailable}
	if got := f.mustGet(t, "run-live", "writer-live"); got.Availability != signet.InvocationTranscriptUnavailable || got.Live != nil {
		t.Fatalf("no handoff running = %#v", got)
	}
}

// The live reader is never asked when a claim exists, so an ended writer
// reads as before; nor for an invocation the run doesn't record as an
// implementation attempt; nor for another run's invocation, which stays
// not found.
func TestInvocationTranscriptReadsOnlyARunningWriterLive(t *testing.T) {
	t.Parallel()
	f := newTranscriptFixture(t)
	live := &stubLive{availability: signet.InvocationTranscriptLive, live: &signet.LiveTranscript{Records: []byte("leak\n")}}
	*f.live = live

	digest := f.storeBytes(t, []byte("stream\n"))
	f.seedWriter(t, "run-ended", "writer-ended",
		transcriptClaimFixture("writer-ended", "agent-transcript-writer-ended", digest, domain.EvidenceMediaApplicationJSONL))
	if got := f.mustGet(t, "run-ended", "writer-ended"); got.Availability != signet.InvocationTranscriptAvailable || got.Live != nil {
		t.Fatalf("ended writer = %#v", got)
	}
	absent := domain.Digest("sha256:" + string(bytes.Repeat([]byte("d"), 64)))
	f.seedWriter(t, "run-gone", "writer-gone",
		transcriptClaimFixture("writer-gone", "agent-transcript-writer-gone", absent, domain.EvidenceMediaApplicationJSONL))
	if got := f.mustGet(t, "run-gone", "writer-gone"); got.Availability != signet.InvocationTranscriptUnavailable || got.Live != nil {
		t.Fatalf("ended writer whose bytes are gone = %#v", got)
	}

	for _, name := range []string{"review", "verification", "specification"} {
		runID := domain.RunID("run-" + name)
		invocation := domain.InvocationID(name + "-invocation")
		f.seedStageInvocation(t, runID, name, invocation)
		if got := f.mustGet(t, runID, invocation); got.Availability != signet.InvocationTranscriptUnavailable || got.Live != nil {
			t.Errorf("%s invocation = %#v, want unavailable", name, got)
		}
	}

	f.seedWriter(t, "run-mine", "writer-mine")
	f.seedWriter(t, "run-other", "writer-other")
	if code, body := f.get(t, "run-mine", "writer-other"); code != http.StatusNotFound {
		t.Errorf("another run's invocation = %d %s, want 404", code, body)
	}
	if asks := live.asked(); len(asks) != 0 {
		t.Errorf("live reader asked %q, want never", asks)
	}
}

// A malformed after, and one the reader refuses as off its records, are a
// 400; a reader failure is a 500 with nothing served.
func TestInvocationTranscriptRefusesABadOffset(t *testing.T) {
	t.Parallel()
	f := newTranscriptFixture(t)
	f.seedWriter(t, "run-offset", "writer-offset")
	live := &stubLive{availability: signet.InvocationTranscriptLive, live: &signet.LiveTranscript{}}
	*f.live = live
	for _, query := range []string{"?after=", "?after=x", "?after=-1", "?after=1.5", "?after=1&after=2"} {
		if code, body := f.getQuery(t, "run-offset", "writer-offset", query); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", query, code, body)
		}
	}
	if asks := live.asked(); len(asks) != 0 {
		t.Errorf("reader asked %q for a malformed offset", asks)
	}
	*f.live = &stubLive{err: fmt.Errorf("wrapped: %w", signet.ErrLiveTranscriptOffset)}
	if code, body := f.getQuery(t, "run-offset", "writer-offset", "?after=3"); code != http.StatusBadRequest {
		t.Errorf("offset off the records = %d %s, want 400", code, body)
	}
	*f.live = &stubLive{err: errors.New("reader failed")}
	if code, body := f.get(t, "run-offset", "writer-offset"); code != http.StatusInternalServerError {
		t.Errorf("reader failure = %d %s, want 500", code, body)
	}
}

// seedStageInvocation records an admitted invocation as the one attempt of
// a stage named stageName.
func (f transcriptFixture) seedStageInvocation(t *testing.T, runID domain.RunID, stageName string, invocation domain.InvocationID) {
	t.Helper()
	run := corpusRun(runID, corpusAttempt(runID, invocation))
	run.Stages[0].Name = stageName
	f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(context.Background(), run) })
	f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
}
