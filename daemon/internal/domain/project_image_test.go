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

func projectImageEnvironmentFixture() *domain.ProjectImageEnvironment {
	return &domain.ProjectImageEnvironment{
		PackageJSONSHA256: strings.Repeat("12", 32),
		PackageLockSHA256: strings.Repeat("34", 32),
		PreparationDigest: domain.Digest("sha256:" + strings.Repeat("56", 32)),
	}
}

// projectImageWithEnvironment rebuilds the fixture as a record that carries
// environment evidence, through the constructor so its ID is the v2 address.
func projectImageWithEnvironment(t *testing.T) domain.ProjectImage {
	t.Helper()
	legacy := projectImageFixture(t)
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: legacy.Repository, RepositoryID: legacy.RepositoryID,
		CommitSHA: legacy.CommitSHA, RecipeDigest: legacy.RecipeDigest,
		PreparationCommand: legacy.PreparationCommand,
		BaseImageRef:       legacy.BaseImageRef, ImageRef: legacy.ImageRef,
		Environment: projectImageEnvironmentFixture(),
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

// The ID preimage is versioned by whether the record carries an environment:
// a legacy record keeps the address it was stored under, and the environment
// is part of the address whenever it exists.
func TestProjectImageIDBindsTheEnvironment(t *testing.T) {
	legacy := projectImageFixture(t)
	const recordedLegacyID = "sha256:7e7a5c42548e88175b869c057c4ad4bb221dc0069e4caf6818dfe7c4af926349"
	if legacy.ID != recordedLegacyID {
		t.Fatalf("legacy ID = %s, want the pre-environment address %s", legacy.ID, recordedLegacyID)
	}
	withEnvironment := projectImageWithEnvironment(t)
	if withEnvironment.ID == legacy.ID {
		t.Fatal("a record with an environment kept the legacy address")
	}

	stripped := withEnvironment
	stripped.Environment = nil
	if err := stripped.Validate(); !errors.Is(err, domain.ErrProjectImageInconsistent) {
		t.Fatalf("stripped environment Validate() = %v, want ErrProjectImageInconsistent", err)
	}
	grafted := legacy
	grafted.Environment = projectImageEnvironmentFixture()
	if err := grafted.Validate(); !errors.Is(err, domain.ErrProjectImageInconsistent) {
		t.Fatalf("grafted environment Validate() = %v, want ErrProjectImageInconsistent", err)
	}
	for name, mutate := range map[string]func(*domain.ProjectImageEnvironment){
		"package.json":      func(e *domain.ProjectImageEnvironment) { e.PackageJSONSHA256 = strings.Repeat("ab", 32) },
		"package-lock.json": func(e *domain.ProjectImageEnvironment) { e.PackageLockSHA256 = strings.Repeat("ab", 32) },
		"preparation": func(e *domain.ProjectImageEnvironment) {
			e.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("ab", 32))
		},
	} {
		altered := withEnvironment
		altered.Environment = projectImageEnvironmentFixture()
		mutate(altered.Environment)
		if err := altered.Validate(); !errors.Is(err, domain.ErrProjectImageInconsistent) {
			t.Fatalf("altered %s Validate() = %v, want ErrProjectImageInconsistent", name, err)
		}
	}
}

func TestProjectImageEnvironmentValidation(t *testing.T) {
	for name, mutate := range map[string]func(*domain.ProjectImageEnvironment){
		"empty package.json hash":   func(e *domain.ProjectImageEnvironment) { e.PackageJSONSHA256 = "" },
		"prefixed package.json":     func(e *domain.ProjectImageEnvironment) { e.PackageJSONSHA256 = "sha256:" + e.PackageJSONSHA256 },
		"uppercase lock hash":       func(e *domain.ProjectImageEnvironment) { e.PackageLockSHA256 = strings.Repeat("AB", 32) },
		"abbreviated lock hash":     func(e *domain.ProjectImageEnvironment) { e.PackageLockSHA256 = "1234" },
		"empty preparation digest":  func(e *domain.ProjectImageEnvironment) { e.PreparationDigest = "" },
		"bare preparation digest":   func(e *domain.ProjectImageEnvironment) { e.PreparationDigest = domain.Digest(strings.Repeat("56", 32)) },
		"malformed preparation hex": func(e *domain.ProjectImageEnvironment) { e.PreparationDigest = "sha256:abc" },
	} {
		t.Run(name, func(t *testing.T) {
			legacy := projectImageFixture(t)
			environment := projectImageEnvironmentFixture()
			mutate(environment)
			_, err := domain.NewProjectImage(domain.ProjectImageInput{
				Repository: legacy.Repository, RepositoryID: legacy.RepositoryID,
				CommitSHA: legacy.CommitSHA, RecipeDigest: legacy.RecipeDigest,
				PreparationCommand: legacy.PreparationCommand,
				BaseImageRef:       legacy.BaseImageRef, ImageRef: legacy.ImageRef,
				Environment: environment,
			})
			if !errors.Is(err, domain.ErrProjectImageInvalid) {
				t.Fatalf("NewProjectImage() = %v, want ErrProjectImageInvalid", err)
			}
		})
	}
}

func TestNewProjectImageDetachesEnvironment(t *testing.T) {
	legacy := projectImageFixture(t)
	environment := projectImageEnvironmentFixture()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: legacy.Repository, RepositoryID: legacy.RepositoryID,
		CommitSHA: legacy.CommitSHA, RecipeDigest: legacy.RecipeDigest,
		PreparationCommand: legacy.PreparationCommand,
		BaseImageRef:       legacy.BaseImageRef, ImageRef: legacy.ImageRef,
		Environment: environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	environment.PackageJSONSHA256 = strings.Repeat("ff", 32)
	if err := image.Validate(); err != nil {
		t.Fatalf("project image retained the caller-owned environment: %v", err)
	}
}

func TestProjectImageEnvironmentDigestCoversEveryField(t *testing.T) {
	base, err := projectImageEnvironmentFixture().Digest()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.ProjectImageEnvironment){
		"package.json":      func(e *domain.ProjectImageEnvironment) { e.PackageJSONSHA256 = strings.Repeat("ab", 32) },
		"package-lock.json": func(e *domain.ProjectImageEnvironment) { e.PackageLockSHA256 = strings.Repeat("ab", 32) },
		"preparation": func(e *domain.ProjectImageEnvironment) {
			e.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("ab", 32))
		},
	} {
		environment := projectImageEnvironmentFixture()
		mutate(environment)
		got, err := environment.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Fatalf("environment digest ignores %s", name)
		}
	}
}

func TestProjectImageAdmissibleAt(t *testing.T) {
	const otherBase = "0123456789abcdef0123456789abcdef01234567"
	image := projectImageWithEnvironment(t)
	current := image.Environment.PreparationDigest
	// compatible is a base other than the build commit whose inputs all
	// equal the record: a source-only advance.
	compatible := func() domain.ProjectImageBaseInputs {
		return domain.ProjectImageBaseInputs{
			CommitSHA:         otherBase,
			PackageJSONSHA256: image.Environment.PackageJSONSHA256,
			PackageLockSHA256: image.Environment.PackageLockSHA256,
			RecipeDigest:      image.RecipeDigest,
		}
	}
	if err := image.AdmissibleAt(compatible(), current); err != nil {
		t.Fatalf("source-only base refused: %v", err)
	}
	// A base that declares no recipe in its tree contradicts nothing: the
	// image runs the recipe it baked, which approval still gates.
	undeclared := compatible()
	undeclared.RecipeDigest = ""
	if err := image.AdmissibleAt(undeclared, current); err != nil {
		t.Fatalf("base without an in-tree recipe refused: %v", err)
	}

	refusals := []struct {
		name        string
		mutate      func(*domain.ProjectImageBaseInputs)
		preparation domain.Digest
		names       string
	}{
		{"changed package.json", func(b *domain.ProjectImageBaseInputs) {
			b.PackageJSONSHA256 = strings.Repeat("ab", 32)
		}, current, "package.json"},
		{"absent package.json", func(b *domain.ProjectImageBaseInputs) { b.PackageJSONSHA256 = "" }, current, "package.json"},
		{"changed package-lock.json", func(b *domain.ProjectImageBaseInputs) {
			b.PackageLockSHA256 = strings.Repeat("ab", 32)
		}, current, "package-lock.json"},
		{"absent package-lock.json", func(b *domain.ProjectImageBaseInputs) { b.PackageLockSHA256 = "" }, current, "package-lock.json"},
		{"npm-shrinkwrap.json present", func(b *domain.ProjectImageBaseInputs) {
			b.UnsupportedInputs = []string{"npm-shrinkwrap.json"}
		}, current, "npm-shrinkwrap.json"},
		{".npmrc present", func(b *domain.ProjectImageBaseInputs) {
			b.UnsupportedInputs = []string{".npmrc"}
		}, current, ".npmrc"},
		{"changed recipe", func(b *domain.ProjectImageBaseInputs) {
			b.RecipeDigest = domain.Digest("sha256:" + strings.Repeat("ab", 32))
		}, current, "recipe"},
		{
			"changed preparation", func(*domain.ProjectImageBaseInputs) {},
			domain.Digest("sha256:" + strings.Repeat("ab", 32)), "preparation",
		},
		{"unknown preparation", func(*domain.ProjectImageBaseInputs) {}, "", "preparation"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			base := compatible()
			tc.mutate(&base)
			err := image.AdmissibleAt(base, tc.preparation)
			if !errors.Is(err, domain.ErrProjectImageIncompatible) {
				t.Fatalf("AdmissibleAt() = %v, want ErrProjectImageIncompatible", err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("refusal %q does not name %q", err, tc.names)
			}
			// The build commit stays admissible whatever else is observed:
			// that is the exact-commit rule, unchanged.
			base.CommitSHA = image.CommitSHA
			if err := image.AdmissibleAt(base, tc.preparation); err != nil {
				t.Fatalf("build commit refused: %v", err)
			}
		})
	}
}

// A record built before environment evidence existed proves nothing about
// another commit, so it stays usable exactly where it always was.
func TestLegacyProjectImageIsAdmissibleOnlyAtItsBuildCommit(t *testing.T) {
	legacy := projectImageFixture(t)
	withEnvironment := projectImageWithEnvironment(t)
	current := withEnvironment.Environment.PreparationDigest
	if err := legacy.AdmissibleAt(
		domain.ProjectImageBaseInputs{CommitSHA: legacy.CommitSHA}, current,
	); err != nil {
		t.Fatalf("legacy record refused at its build commit: %v", err)
	}
	err := legacy.AdmissibleAt(domain.ProjectImageBaseInputs{
		CommitSHA:         "0123456789abcdef0123456789abcdef01234567",
		PackageJSONSHA256: withEnvironment.Environment.PackageJSONSHA256,
		PackageLockSHA256: withEnvironment.Environment.PackageLockSHA256,
		RecipeDigest:      legacy.RecipeDigest,
	}, current)
	if !errors.Is(err, domain.ErrProjectImageIncompatible) ||
		!strings.Contains(err.Error(), "rebuild") {
		t.Fatalf("legacy record at another commit = %v, want a rebuild refusal", err)
	}
}
