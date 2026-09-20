package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func pubDigest(seed string) domain.Digest {
	return domain.Digest(contentaddr.Sum([]byte(seed)))
}

// validPublicationAuthoringInput is the shared valid input; reviewer_notes is
// present. Cases derive from it and mutate one field.
func validPublicationAuthoringInput() domain.PublicationAuthoringInput {
	notes := "Reviewer confirmed the diff touches only docs."
	return domain.PublicationAuthoringInput{
		RunID:          "run-1",
		Title:          "Publish the closure summary",
		Body:           "The run closed issue #42 with an evidence-backed pull request.",
		ReviewerNotes:  &notes,
		OutcomeSummary: "All required checks green; independent review clean.",
		EvidenceRefs: []domain.PublicationEvidenceReference{
			{ArtifactID: "art-1", Digest: pubDigest("art-1")},
			{ArtifactID: "art-2", Digest: pubDigest("art-2")},
		},
		Producer: domain.PublicationProducer{
			Site: "explain", Producer: "claude-opus/high", InputDigest: pubDigest("inputs"),
		},
		SensitivityClass: domain.SensitivityNormal,
		CreatedAt:        time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func mustPublicationAuthoring(t *testing.T, in domain.PublicationAuthoringInput) domain.PublicationAuthoring {
	t.Helper()
	artifact, err := domain.NewPublicationAuthoring(in)
	if err != nil {
		t.Fatalf("NewPublicationAuthoring: %v", err)
	}
	return artifact
}

// TestPublicationAuthoringGolden pins the canonical encoding: one fixture with
// reviewer_notes absent (explicit null) and one with it present. Each fixture is
// a valid value, so it doubles as a validation-positive case.
func TestPublicationAuthoringGolden(t *testing.T) {
	t.Parallel()

	withoutNotes := validPublicationAuthoringInput()
	withoutNotes.ReviewerNotes = nil
	body, err := json.MarshalIndent(mustPublicationAuthoring(t, withoutNotes), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden.Assert(t, "publication_authoring", append(body, '\n'))

	withNotes := validPublicationAuthoringInput()
	withNotesBody, err := json.MarshalIndent(mustPublicationAuthoring(t, withNotes), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden.Assert(t, "publication_authoring_with_notes", append(withNotesBody, '\n'))

	// The golden must not carry a publish_eligible key: this type is not a
	// domain.Artifact and holds no trust bit.
	if strings.Contains(string(withNotesBody), "publish_eligible") {
		t.Fatalf("golden unexpectedly contains publish_eligible: %s", withNotesBody)
	}
}

// TestPublicationAuthoringConstructorRejects covers the input the constructor
// itself refuses.
func TestPublicationAuthoringConstructorRejects(t *testing.T) {
	t.Parallel()
	oversize := func(n int) string { return strings.Repeat("a", n) }
	longNotes := oversize(domain.MaxPublicationAuthoringReviewerNotesBytes + 1)
	manyRefs := make([]domain.PublicationEvidenceReference, domain.MaxPublicationEvidenceRefs+1)
	for i := range manyRefs {
		id := domain.ArtifactID("art-" + strings.Repeat("x", i+1))
		manyRefs[i] = domain.PublicationEvidenceReference{ArtifactID: id, Digest: pubDigest(string(id))}
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*domain.PublicationAuthoringInput)
		wantErr error
	}{
		{"empty run_id", func(in *domain.PublicationAuthoringInput) { in.RunID = "" }, domain.ErrEmptyID},
		{"empty title", func(in *domain.PublicationAuthoringInput) { in.Title = "" }, domain.ErrEmptyField},
		{"empty body", func(in *domain.PublicationAuthoringInput) { in.Body = "" }, domain.ErrEmptyField},
		{"empty summary", func(in *domain.PublicationAuthoringInput) { in.OutcomeSummary = "" }, domain.ErrEmptyField},
		{"title over bound", func(in *domain.PublicationAuthoringInput) {
			in.Title = oversize(domain.MaxPublicationAuthoringTitleBytes + 1)
		}, domain.ErrPublicationAuthoringInconsistent},
		{"body over bound", func(in *domain.PublicationAuthoringInput) {
			in.Body = oversize(domain.MaxPublicationAuthoringBodyBytes + 1)
		}, domain.ErrPublicationAuthoringInconsistent},
		{"summary over bound", func(in *domain.PublicationAuthoringInput) {
			in.OutcomeSummary = oversize(domain.MaxPublicationAuthoringOutcomeSummaryBytes + 1)
		}, domain.ErrPublicationAuthoringInconsistent},
		{"reviewer_notes over bound", func(in *domain.PublicationAuthoringInput) {
			in.ReviewerNotes = &longNotes
		}, domain.ErrPublicationAuthoringInconsistent},
		{"invalid utf8 body", func(in *domain.PublicationAuthoringInput) {
			in.Body = "bad\xff"
		}, domain.ErrPublicationAuthoringInconsistent},
		{"too many references", func(in *domain.PublicationAuthoringInput) {
			in.EvidenceRefs = manyRefs
		}, domain.ErrPublicationAuthoringInconsistent},
		{"duplicate reference", func(in *domain.PublicationAuthoringInput) {
			in.EvidenceRefs = []domain.PublicationEvidenceReference{
				{ArtifactID: "art-1", Digest: pubDigest("art-1")},
				{ArtifactID: "art-1", Digest: pubDigest("art-1")},
			}
		}, domain.ErrDuplicate},
		{"reference empty id", func(in *domain.PublicationAuthoringInput) {
			in.EvidenceRefs = []domain.PublicationEvidenceReference{{ArtifactID: "", Digest: pubDigest("x")}}
		}, domain.ErrEmptyID},
		{"reference empty digest", func(in *domain.PublicationAuthoringInput) {
			in.EvidenceRefs = []domain.PublicationEvidenceReference{{ArtifactID: "art-1", Digest: ""}}
		}, domain.ErrEmptyField},
		{"empty site", func(in *domain.PublicationAuthoringInput) { in.Producer.Site = "" }, domain.ErrEmptyField},
		{"empty producer", func(in *domain.PublicationAuthoringInput) { in.Producer.Producer = "" }, domain.ErrEmptyField},
		{"malformed input digest", func(in *domain.PublicationAuthoringInput) {
			in.Producer.InputDigest = "nope"
		}, domain.ErrPublicationAuthoringInconsistent},
		{"invalid sensitivity class", func(in *domain.PublicationAuthoringInput) {
			in.SensitivityClass = domain.SensitivityClass("secret")
		}, domain.ErrInvalidSensitivityClass},
		{"zero created_at", func(in *domain.PublicationAuthoringInput) {
			in.CreatedAt = time.Time{}
		}, domain.ErrMissingTimestamp},
		{"non-UTC created_at", func(in *domain.PublicationAuthoringInput) {
			in.CreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validPublicationAuthoringInput()
			tc.mutate(&in)
			_, err := domain.NewPublicationAuthoring(in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewPublicationAuthoring err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestPublicationAuthoringValidateBackstops covers the checks that only a value
// bypassing the constructor can reach: a wrong encoding version and a digest
// that disagrees with the content.
func TestPublicationAuthoringValidateBackstops(t *testing.T) {
	t.Parallel()

	wrongVersion := mustPublicationAuthoring(t, validPublicationAuthoringInput())
	wrongVersion.EncodingVersion = 2
	if err := wrongVersion.Validate(); !errors.Is(err, domain.ErrPublicationAuthoringInconsistent) {
		t.Fatalf("wrong encoding version err = %v, want inconsistent", err)
	}

	tampered := mustPublicationAuthoring(t, validPublicationAuthoringInput())
	tampered.Title = "A different title that the digest does not cover"
	if err := tampered.Validate(); !errors.Is(err, domain.ErrPublicationAuthoringDigestMismatch) {
		t.Fatalf("tampered content err = %v, want digest mismatch", err)
	}

	badDigest := mustPublicationAuthoring(t, validPublicationAuthoringInput())
	badDigest.Digest = "not-a-digest"
	if err := badDigest.Validate(); !errors.Is(err, domain.ErrPublicationAuthoringDigestMismatch) {
		t.Fatalf("malformed digest err = %v, want digest mismatch", err)
	}
}

// TestPublicationAuthoringDecodeRejects covers the decode-boundary refusals:
// an unknown JSON field, trailing data, and a payload over the byte cap.
func TestPublicationAuthoringDecodeRejects(t *testing.T) {
	t.Parallel()

	valid := mustPublicationAuthoring(t, validPublicationAuthoringInput())
	body, err := valid.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := domain.DecodePublicationAuthoring(body); err != nil {
		t.Fatalf("decode of valid body: %v", err)
	}

	unknown := strings.Replace(string(body), `"title":`, `"unexpected":"x","title":`, 1)
	if _, err := domain.DecodePublicationAuthoring([]byte(unknown)); err == nil {
		t.Fatal("decode with unknown field: want error")
	}

	if _, err := domain.DecodePublicationAuthoring(append(append([]byte{}, body...), '{')); err == nil {
		t.Fatal("decode with trailing data: want error")
	}

	oversize := make([]byte, domain.MaxPublicationAuthoringBytes+1)
	for i := range oversize {
		oversize[i] = ' '
	}
	if _, err := domain.DecodePublicationAuthoring(oversize); err == nil {
		t.Fatal("decode over byte cap: want error")
	}
}

// TestPublicationAuthoringConstructorClonesInputs proves the constructor copies
// the mutable inputs (the reviewer-notes pointer and the reference slice), so a
// caller that reuses its input cannot mutate the digest-addressed artifact after
// its digest was computed.
func TestPublicationAuthoringConstructorClonesInputs(t *testing.T) {
	t.Parallel()
	notes := "original note"
	refs := []domain.PublicationEvidenceReference{{ArtifactID: "art-1", Digest: pubDigest("art-1")}}
	in := validPublicationAuthoringInput()
	in.ReviewerNotes = &notes
	in.EvidenceRefs = refs

	artifact := mustPublicationAuthoring(t, in)

	// Mutate the caller's originals through the shared pointer and slice.
	notes = "mutated note"
	refs[0].Digest = pubDigest("tampered")

	if artifact.ReviewerNotes == nil || *artifact.ReviewerNotes != "original note" {
		t.Fatalf("reviewer_notes reflected caller mutation: %v", artifact.ReviewerNotes)
	}
	if artifact.EvidenceRefs[0].Digest != pubDigest("art-1") {
		t.Fatalf("evidence ref reflected caller mutation: %q", artifact.EvidenceRefs[0].Digest)
	}
	// The artifact still validates: its content matches the digest computed at
	// construction, unchanged by the caller's mutation.
	if err := artifact.Validate(); err != nil {
		t.Fatalf("artifact invalid after caller mutated its inputs: %v", err)
	}
}

// TestPublicationAuthoringEncodeCapsEncodedSize proves the persist boundary
// rejects a body that fits its raw byte bound but expands past the decode cap
// once json.Marshal escapes it: a valid-UTF-8 control byte becomes a six-byte
// \uXXXX escape. Without this, Encode would return, and a store would persist,
// an immutable row that DecodePublicationAuthoring can never read back.
func TestPublicationAuthoringEncodeCapsEncodedSize(t *testing.T) {
	t.Parallel()
	in := validPublicationAuthoringInput()
	// A body at the raw bound made entirely of U+0001 escapes to ~6x its size,
	// well over the 128 KiB encoded cap.
	in.Body = strings.Repeat("\x01", domain.MaxPublicationAuthoringBodyBytes)
	artifact, err := domain.NewPublicationAuthoring(in)
	if err != nil {
		t.Fatalf("constructor rejected a within-bound control-byte body: %v", err)
	}
	if _, err := artifact.Encode(); !errors.Is(err, domain.ErrPublicationAuthoringInconsistent) {
		t.Fatalf("Encode err = %v, want ErrPublicationAuthoringInconsistent", err)
	}
}

// TestPublicationAuthoringDigestStable proves the digest is deterministic, that
// a nil and an empty reference list give the same digest, and that changing any
// field changes it.
func TestPublicationAuthoringDigestStable(t *testing.T) {
	t.Parallel()

	base := mustPublicationAuthoring(t, validPublicationAuthoringInput())
	again := mustPublicationAuthoring(t, validPublicationAuthoringInput())
	if base.Digest != again.Digest {
		t.Fatalf("same inputs gave different digests: %q vs %q", base.Digest, again.Digest)
	}

	nilRefs := validPublicationAuthoringInput()
	nilRefs.EvidenceRefs = nil
	emptyRefs := validPublicationAuthoringInput()
	emptyRefs.EvidenceRefs = []domain.PublicationEvidenceReference{}
	if got, want := mustPublicationAuthoring(t, nilRefs).Digest, mustPublicationAuthoring(t, emptyRefs).Digest; got != want {
		t.Fatalf("nil and empty reference lists gave different digests: %q vs %q", got, want)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*domain.PublicationAuthoringInput)
	}{
		{"run_id", func(in *domain.PublicationAuthoringInput) { in.RunID = "run-2" }},
		{"title", func(in *domain.PublicationAuthoringInput) { in.Title = "Other title" }},
		{"body", func(in *domain.PublicationAuthoringInput) { in.Body = "Other body prose." }},
		{"reviewer_notes", func(in *domain.PublicationAuthoringInput) { in.ReviewerNotes = nil }},
		{"outcome_summary", func(in *domain.PublicationAuthoringInput) { in.OutcomeSummary = "Other summary." }},
		{"evidence_refs", func(in *domain.PublicationAuthoringInput) {
			in.EvidenceRefs = []domain.PublicationEvidenceReference{{ArtifactID: "art-9", Digest: pubDigest("art-9")}}
		}},
		{"producer.site", func(in *domain.PublicationAuthoringInput) { in.Producer.Site = "other-site" }},
		{"producer.producer", func(in *domain.PublicationAuthoringInput) { in.Producer.Producer = "other-model" }},
		{"producer.input_digest", func(in *domain.PublicationAuthoringInput) { in.Producer.InputDigest = pubDigest("other-inputs") }},
		{"sensitivity_class", func(in *domain.PublicationAuthoringInput) { in.SensitivityClass = domain.SensitivityHigh }},
		{"created_at", func(in *domain.PublicationAuthoringInput) {
			in.CreatedAt = time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validPublicationAuthoringInput()
			tc.mutate(&in)
			if mustPublicationAuthoring(t, in).Digest == base.Digest {
				t.Fatalf("changing %s did not change the digest", tc.name)
			}
		})
	}
}
