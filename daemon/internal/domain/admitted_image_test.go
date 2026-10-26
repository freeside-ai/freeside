package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestAdmittedImagePointerValidate(t *testing.T) {
	t.Parallel()
	valid := domain.AdmittedImagePointer{
		RepositoryID: 1278475858, Repository: "freeasinbird/gh-imgup",
		Harness:    domain.HarnessClientClaudeCode,
		ImageID:    domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		ImageRef:   domain.ImageRef("example.test/project@sha256:" + strings.Repeat("cd", 32)),
		AdvancedAt: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid pointer: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*domain.AdmittedImagePointer)
		want   error
	}{
		"repository id": {func(p *domain.AdmittedImagePointer) { p.RepositoryID = 0 }, domain.ErrNonPositive},
		"repository": {
			func(p *domain.AdmittedImagePointer) { p.Repository = "../x" },
			domain.ErrAdmittedImagePointerInvalid,
		},
		"harness": {func(p *domain.AdmittedImagePointer) { p.Harness = "" }, domain.ErrInvalidHarnessClientKind},
		"image id": {
			func(p *domain.AdmittedImagePointer) { p.ImageID = "sha256:short" },
			domain.ErrAdmittedImagePointerInvalid,
		},
		"advanced at zero": {
			func(p *domain.AdmittedImagePointer) { p.AdvancedAt = time.Time{} },
			domain.ErrMissingTimestamp,
		},
		"advanced at local": {func(p *domain.AdmittedImagePointer) {
			p.AdvancedAt = p.AdvancedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
	} {
		pointer := valid
		tc.mutate(&pointer)
		if err := pointer.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", name, err, tc.want)
		}
	}
	pointer := valid
	pointer.ImageRef = "not a ref"
	if err := pointer.Validate(); err == nil {
		t.Error("invalid image_ref: Validate = nil")
	}
}
