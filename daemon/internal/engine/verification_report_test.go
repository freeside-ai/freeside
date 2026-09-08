package engine

import (
	"bytes"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestLoadVerificationReportBindsCountInvocationAndBytes(t *testing.T) {
	t.Parallel()
	raw := []byte("verifier bytes")
	artifact := domain.Artifact{
		Type: domain.ArtifactKindVerificationReport, Digest: domain.Digest(contentaddr.Sum(raw)),
		Provenance: domain.Provenance{ProducerInvocationID: "verify-1"},
	}
	store := remediationArtifactStore{body: raw}
	got, err := loadVerificationReport(store, []domain.Artifact{artifact}, "verify-1")
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("load changed bytes: %q, %v", got, err)
	}
	for name, records := range map[string][]domain.Artifact{
		"missing":            nil,
		"duplicate":          {artifact, artifact},
		"foreign invocation": {{Type: artifact.Type, Digest: artifact.Digest, Provenance: domain.Provenance{ProducerInvocationID: "foreign"}}},
		"foreign digest":     {{Type: artifact.Type, Digest: "sha256:foreign", Provenance: artifact.Provenance}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadVerificationReport(store, records, "verify-1"); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("got %v, want contradiction", err)
			}
		})
	}
	oversized := bytes.Repeat([]byte("x"), 1<<20+1)
	artifact.Digest = domain.Digest(contentaddr.Sum(oversized))
	if _, err := loadVerificationReport(remediationArtifactStore{body: oversized}, []domain.Artifact{artifact}, "verify-1"); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("oversized report = %v", err)
	}
	readFailure := errors.New("read failed")
	if _, err := loadVerificationReport(remediationArtifactStore{readErr: readFailure}, []domain.Artifact{artifact}, "verify-1"); !errors.Is(err, readFailure) {
		t.Fatalf("read failure was hidden: %v", err)
	}
	if _, err := loadVerificationReport(remediationArtifactStore{body: raw, closeErr: readFailure}, []domain.Artifact{artifact}, "verify-1"); !errors.Is(err, readFailure) {
		t.Fatalf("close failure was hidden: %v", err)
	}
}
