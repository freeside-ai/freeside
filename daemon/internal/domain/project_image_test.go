package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func projectImageFixture(t *testing.T) domain.ProjectImage {
	t.Helper()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository:         "freeasinbird/gh-imgup",
		RepositoryID:       1278475858,
		CommitSHA:          "6ab4e3dff2be53f74bde9b8b3150290775152f9f",
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("ef", 32)),
		PreparationCommand: []string{"/usr/local/bin/freeside-project-prepare"},
		BaseImageRef: domain.ImageRef(
			"ghcr.io/freeside-ai/agent-claude@sha256:" + strings.Repeat("ab", 32)),
		ImageRef: domain.ImageRef(
			"127.0.0.1:5100/freeside-project-freeasinbird-gh-imgup@sha256:" + strings.Repeat("cd", 32)),
	})
	if err != nil {
		t.Fatalf("NewProjectImage: %v", err)
	}
	return image
}

func TestProjectImageValidation(t *testing.T) {
	base := projectImageFixture(t)
	cases := []struct {
		name   string
		mutate func(*domain.ProjectImage)
		want   error
	}{
		{"empty repository", func(p *domain.ProjectImage) { p.Repository = "" }, domain.ErrProjectImageInvalid},
		{"option-shaped owner", func(p *domain.ProjectImage) { p.Repository = "-owner/repo" }, domain.ErrProjectImageInvalid},
		{"traversing repository", func(p *domain.ProjectImage) { p.Repository = "owner/../repo" }, domain.ErrProjectImageInvalid},
		{"missing repository id", func(p *domain.ProjectImage) { p.RepositoryID = 0 }, domain.ErrNonPositive},
		{"abbreviated commit", func(p *domain.ProjectImage) { p.CommitSHA = "6ab4e3d" }, domain.ErrProjectImageInvalid},
		{"uppercase commit", func(p *domain.ProjectImage) { p.CommitSHA = strings.ToUpper(p.CommitSHA) }, domain.ErrProjectImageInvalid},
		{"empty recipe digest", func(p *domain.ProjectImage) { p.RecipeDigest = "" }, domain.ErrProjectImageInvalid},
		{"abbreviated recipe digest", func(p *domain.ProjectImage) {
			p.RecipeDigest = "sha256:abc"
		}, domain.ErrProjectImageInvalid},
		{"empty preparation", func(p *domain.ProjectImage) { p.PreparationCommand = nil }, domain.ErrEmptyField},
		{"nul preparation token", func(p *domain.ProjectImage) {
			p.PreparationCommand = []string{"prepare", "bad\x00token"}
		}, domain.ErrProjectImageInvalid},
		{"tagged base image", func(p *domain.ProjectImage) {
			p.BaseImageRef = "example.test/agent:latest"
		}, domain.ErrImageNotDigestPinned},
		{"tagged result image", func(p *domain.ProjectImage) {
			p.ImageRef = "example.test/project:latest"
		}, domain.ErrImageNotDigestPinned},
		{"forged id", func(p *domain.ProjectImage) { p.ID = "sha256:forged" }, domain.ErrProjectImageInconsistent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := base
			got.PreparationCommand = append([]string{}, base.PreparationCommand...)
			tc.mutate(&got)
			if err := got.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewProjectImageDetachesPreparationCommand(t *testing.T) {
	command := []string{"/usr/local/bin/freeside-project-prepare"}
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "owner/repo", RepositoryID: 1,
		CommitSHA:    "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)), PreparationCommand: command,
		BaseImageRef: domain.ImageRef("example.test/base@sha256:" + strings.Repeat("a", 64)),
		ImageRef:     domain.ImageRef("example.test/project@sha256:" + strings.Repeat("b", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	command[0] = "mutated"
	if image.PreparationCommand[0] != "/usr/local/bin/freeside-project-prepare" {
		t.Fatal("project image retained caller-owned preparation command")
	}
}
