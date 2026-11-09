package domain_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func goSwiftProjectImageEnvironmentFixture() *domain.ProjectImageEnvironment {
	return &domain.ProjectImageEnvironment{
		Toolchain:             domain.ProjectToolchainGoSwift,
		GoModuleDir:           "daemon",
		SwiftPackageDir:       "app",
		GoModSHA256:           strings.Repeat("78", 32),
		GoSumSHA256:           strings.Repeat("9a", 32),
		PackageSwiftSHA256:    strings.Repeat("bc", 32),
		PackageResolvedSHA256: strings.Repeat("de", 32),
		PreparationDigest:     domain.Digest("sha256:" + strings.Repeat("56", 32)),
	}
}

func TestProjectToolchainMembers(t *testing.T) {
	for toolchain, want := range map[domain.ProjectToolchain][]domain.ProjectToolchain{
		domain.ProjectToolchainNPM:     {domain.ProjectToolchainNPM},
		domain.ProjectToolchainGo:      {domain.ProjectToolchainGo},
		domain.ProjectToolchainSwift:   {domain.ProjectToolchainSwift},
		domain.ProjectToolchainGoSwift: {domain.ProjectToolchainGo, domain.ProjectToolchainSwift},
		"":                             nil,
		"uv":                           nil,
	} {
		if got := toolchain.Members(); !slices.Equal(got, want) {
			t.Errorf("%q.Members() = %v, want %v", toolchain, got, want)
		}
	}
	if !domain.ProjectToolchainGoSwift.Includes(domain.ProjectToolchainSwift) ||
		domain.ProjectToolchainGoSwift.Includes(domain.ProjectToolchainNPM) ||
		domain.ProjectToolchainGo.Includes(domain.ProjectToolchainSwift) {
		t.Fatal("Includes disagrees with Members")
	}
}

func TestGoSwiftProjectImageInputLayoutValidate(t *testing.T) {
	for name, layout := range map[string]domain.ProjectImageInputLayout{
		"both nested":      {Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: "daemon", SwiftPackageDir: "app"},
		"both at the root": {Toolchain: domain.ProjectToolchainGoSwift},
	} {
		if err := layout.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, layout := range map[string]domain.ProjectImageInputLayout{
		"escaping module dir": {
			Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: "../daemon", SwiftPackageDir: "app",
		},
		"absolute package dir": {
			Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: "daemon", SwiftPackageDir: "/app",
		},
		"npm with both dirs": {Toolchain: domain.ProjectToolchainNPM, GoModuleDir: "daemon", SwiftPackageDir: "app"},
	} {
		if err := layout.Validate(); !errors.Is(err, domain.ErrProjectImageInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrProjectImageInvalid", name, err)
		}
	}
}

func TestGoSwiftProjectImageEnvironmentValidation(t *testing.T) {
	image, err := goProjectImage(t, goSwiftProjectImageEnvironmentFixture())
	if err != nil {
		t.Fatalf("Go and Swift record refused: %v", err)
	}
	if got := image.InputLayout(); got != (domain.ProjectImageInputLayout{
		Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: "daemon", SwiftPackageDir: "app",
	}) {
		t.Fatalf("InputLayout() = %+v", got)
	}

	for name, mutate := range map[string]func(*domain.ProjectImageEnvironment){
		"missing go.mod":           func(e *domain.ProjectImageEnvironment) { e.GoModSHA256 = "" },
		"missing go.sum":           func(e *domain.ProjectImageEnvironment) { e.GoSumSHA256 = "" },
		"missing Package.swift":    func(e *domain.ProjectImageEnvironment) { e.PackageSwiftSHA256 = "" },
		"missing Package.resolved": func(e *domain.ProjectImageEnvironment) { e.PackageResolvedSHA256 = "" },
		"missing the Go pair": func(e *domain.ProjectImageEnvironment) {
			e.GoModSHA256, e.GoSumSHA256 = "", ""
		},
		"missing the Swift pair": func(e *domain.ProjectImageEnvironment) {
			e.PackageSwiftSHA256, e.PackageResolvedSHA256 = "", ""
		},
		"an npm hash": func(e *domain.ProjectImageEnvironment) {
			e.PackageLockSHA256 = strings.Repeat("34", 32)
		},
		"malformed go.sum hash": func(e *domain.ProjectImageEnvironment) { e.GoSumSHA256 = "1234" },
		"escaping package dir":  func(e *domain.ProjectImageEnvironment) { e.SwiftPackageDir = "../app" },
	} {
		t.Run(name, func(t *testing.T) {
			environment := goSwiftProjectImageEnvironmentFixture()
			mutate(environment)
			if _, err := goProjectImage(t, environment); !errors.Is(err, domain.ErrProjectImageInvalid) {
				t.Fatalf("NewProjectImage() = %v, want ErrProjectImageInvalid", err)
			}
		})
	}
}

// A mixed record's ID and environment digest cover every field of both
// toolchains, and neither collides with a single-toolchain record's: the
// toolchain selects the preimage version.
func TestGoSwiftProjectImageAddressCoversEveryField(t *testing.T) {
	image, err := goProjectImage(t, goSwiftProjectImageEnvironmentFixture())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := image.Environment.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.ProjectImageEnvironment){
		"module dir":       func(e *domain.ProjectImageEnvironment) { e.GoModuleDir = "cmd" },
		"package dir":      func(e *domain.ProjectImageEnvironment) { e.SwiftPackageDir = "apps" },
		"go.mod":           func(e *domain.ProjectImageEnvironment) { e.GoModSHA256 = strings.Repeat("ab", 32) },
		"go.sum":           func(e *domain.ProjectImageEnvironment) { e.GoSumSHA256 = strings.Repeat("ab", 32) },
		"Package.swift":    func(e *domain.ProjectImageEnvironment) { e.PackageSwiftSHA256 = strings.Repeat("ab", 32) },
		"Package.resolved": func(e *domain.ProjectImageEnvironment) { e.PackageResolvedSHA256 = strings.Repeat("ab", 32) },
		"preparation": func(e *domain.ProjectImageEnvironment) {
			e.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("ab", 32))
		},
	} {
		t.Run(name, func(t *testing.T) {
			altered := image
			altered.Environment = goSwiftProjectImageEnvironmentFixture()
			mutate(altered.Environment)
			if err := altered.Validate(); !errors.Is(err, domain.ErrProjectImageInconsistent) {
				t.Fatalf("altered %s Validate() = %v, want ErrProjectImageInconsistent", name, err)
			}
			got, err := altered.Environment.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if got == digest {
				t.Fatalf("environment digest ignores %s", name)
			}
		})
	}
	for name, other := range map[string]*domain.ProjectImageEnvironment{
		"npm":   projectImageEnvironmentFixture(),
		"Go":    goProjectImageEnvironmentFixture(),
		"Swift": swiftProjectImageEnvironmentFixture(),
	} {
		otherDigest, err := other.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if otherDigest == digest {
			t.Fatalf("Go and Swift and %s environments share a digest", name)
		}
	}
}

func TestGoSwiftProjectImageAdmissibleAt(t *testing.T) {
	const otherBase = "0123456789abcdef0123456789abcdef01234567"
	image, err := goProjectImage(t, goSwiftProjectImageEnvironmentFixture())
	if err != nil {
		t.Fatal(err)
	}
	current := image.Environment.PreparationDigest
	compatible := func() domain.ProjectImageBaseInputs {
		return domain.ProjectImageBaseInputs{
			CommitSHA:             otherBase,
			GoModSHA256:           image.Environment.GoModSHA256,
			GoSumSHA256:           image.Environment.GoSumSHA256,
			PackageSwiftSHA256:    image.Environment.PackageSwiftSHA256,
			PackageResolvedSHA256: image.Environment.PackageResolvedSHA256,
			RecipeDigest:          image.RecipeDigest,
		}
	}
	if err := image.AdmissibleAt(compatible(), current); err != nil {
		t.Fatalf("base with both toolchains' manifests equal refused: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*domain.ProjectImageBaseInputs)
		names  string
	}{
		{"changed go.mod", func(b *domain.ProjectImageBaseInputs) {
			b.GoModSHA256 = strings.Repeat("ab", 32)
		}, "daemon/go.mod at the base differs"},
		{"changed go.sum", func(b *domain.ProjectImageBaseInputs) {
			b.GoSumSHA256 = strings.Repeat("ab", 32)
		}, "daemon/go.sum at the base differs"},
		{"changed Package.swift", func(b *domain.ProjectImageBaseInputs) {
			b.PackageSwiftSHA256 = strings.Repeat("ab", 32)
		}, "app/Package.swift at the base differs"},
		{"absent Package.resolved", func(b *domain.ProjectImageBaseInputs) {
			b.PackageResolvedSHA256 = ""
		}, "app/Package.resolved at the base differs"},
		{"Go unsupported input", func(b *domain.ProjectImageBaseInputs) {
			b.UnsupportedInputs = []string{"daemon/vendor"}
		}, "unsupported Go and Swift input daemon/vendor"},
		{"Swift unsupported input", func(b *domain.ProjectImageBaseInputs) {
			b.UnsupportedInputs = []string{"app/.swiftpm/configuration/mirrors.json"}
		}, "unsupported Go and Swift input app/.swiftpm/configuration/mirrors.json"},
		{"inputs observed under the Go layout", func(b *domain.ProjectImageBaseInputs) {
			b.PackageSwiftSHA256, b.PackageResolvedSHA256 = "", ""
		}, "app/Package.swift"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := compatible()
			tc.mutate(&base)
			err := image.AdmissibleAt(base, current)
			if !errors.Is(err, domain.ErrProjectImageIncompatible) {
				t.Fatalf("AdmissibleAt() = %v, want ErrProjectImageIncompatible", err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("refusal %q does not name %q", err, tc.names)
			}
		})
	}

	// ChangedManifests names the Go pair before the Swift pair.
	allChanged := domain.ProjectImageBaseInputs{CommitSHA: otherBase}
	if got := strings.Join(image.Environment.ChangedManifests(allChanged), ","); got !=
		"daemon/go.mod,daemon/go.sum,app/Package.swift,app/Package.resolved" {
		t.Fatalf("ChangedManifests() = %q, want the Go pair then the Swift pair", got)
	}
}
