package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func swiftProjectImageEnvironmentFixture() *domain.ProjectImageEnvironment {
	return &domain.ProjectImageEnvironment{
		Toolchain:             domain.ProjectToolchainSwift,
		SwiftPackageDir:       "app",
		PackageSwiftSHA256:    strings.Repeat("bc", 32),
		PackageResolvedSHA256: strings.Repeat("de", 32),
		PreparationDigest:     domain.Digest("sha256:" + strings.Repeat("56", 32)),
	}
}

func swiftProjectImage(t *testing.T, environment *domain.ProjectImageEnvironment) (domain.ProjectImage, error) {
	t.Helper()
	return goProjectImage(t, environment)
}

func TestSwiftProjectImageInputLayoutValidate(t *testing.T) {
	for name, layout := range map[string]domain.ProjectImageInputLayout{
		"root package":   {Toolchain: domain.ProjectToolchainSwift},
		"nested package": {Toolchain: domain.ProjectToolchainSwift, SwiftPackageDir: "apps/mac"},
	} {
		if err := layout.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, layout := range map[string]domain.ProjectImageInputLayout{
		"npm with a package dir": {Toolchain: domain.ProjectToolchainNPM, SwiftPackageDir: "app"},
		"Go with a package dir": {
			Toolchain: domain.ProjectToolchainGo, GoModuleDir: "daemon", SwiftPackageDir: "app",
		},
		"Swift with a module dir":    {Toolchain: domain.ProjectToolchainSwift, GoModuleDir: "daemon"},
		"Swift with an invalid path": {Toolchain: domain.ProjectToolchainSwift, SwiftPackageDir: "../app"},
		"Swift with an absolute path": {
			Toolchain: domain.ProjectToolchainSwift, SwiftPackageDir: "/app",
		},
	} {
		if err := layout.Validate(); !errors.Is(err, domain.ErrProjectImageInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrProjectImageInvalid", name, err)
		}
	}
}

// The package directory shares the Go module directory's grammar.
func TestValidProjectSwiftPackageDir(t *testing.T) {
	for _, dir := range []string{"", "app", "apps/mac", "a_b/c-d/e.f"} {
		if !domain.ValidProjectSwiftPackageDir(dir) {
			t.Errorf("ValidProjectSwiftPackageDir(%q) = false, want true", dir)
		}
	}
	for _, dir := range []string{".", "..", "./app", "../app", "/app", "app/", ".swiftpm", "has space"} {
		if domain.ValidProjectSwiftPackageDir(dir) {
			t.Errorf("ValidProjectSwiftPackageDir(%q) = true, want false", dir)
		}
	}
}

func TestSwiftProjectImageEnvironmentValidation(t *testing.T) {
	image, err := swiftProjectImage(t, swiftProjectImageEnvironmentFixture())
	if err != nil {
		t.Fatalf("Swift record refused: %v", err)
	}
	if got := image.InputLayout(); got != (domain.ProjectImageInputLayout{
		Toolchain: domain.ProjectToolchainSwift, SwiftPackageDir: "app",
	}) {
		t.Fatalf("InputLayout() = %+v", got)
	}
	root := swiftProjectImageEnvironmentFixture()
	root.SwiftPackageDir = ""
	if _, err := swiftProjectImage(t, root); err != nil {
		t.Fatalf("Swift record with its package at the root refused: %v", err)
	}

	for name, mutate := range map[string]func(*domain.ProjectImageEnvironment){
		"Swift record with npm hashes": func(e *domain.ProjectImageEnvironment) {
			e.PackageJSONSHA256 = strings.Repeat("12", 32)
			e.PackageLockSHA256 = strings.Repeat("34", 32)
		},
		"Swift record with a Go hash": func(e *domain.ProjectImageEnvironment) {
			e.GoModSHA256 = strings.Repeat("78", 32)
		},
		"Swift record with a module dir": func(e *domain.ProjectImageEnvironment) {
			e.GoModuleDir = "daemon"
		},
		"Swift record missing Package.resolved": func(e *domain.ProjectImageEnvironment) {
			e.PackageResolvedSHA256 = ""
		},
		"Swift record missing Package.swift": func(e *domain.ProjectImageEnvironment) {
			e.PackageSwiftSHA256 = ""
		},
		"malformed Package.swift hash": func(e *domain.ProjectImageEnvironment) {
			e.PackageSwiftSHA256 = "1234"
		},
		"escaping package dir": func(e *domain.ProjectImageEnvironment) { e.SwiftPackageDir = "../app" },
		"absolute package dir": func(e *domain.ProjectImageEnvironment) { e.SwiftPackageDir = "/app" },
		"Go record with Swift hashes": func(e *domain.ProjectImageEnvironment) {
			*e = *goProjectImageEnvironmentFixture()
			e.PackageSwiftSHA256 = strings.Repeat("bc", 32)
			e.PackageResolvedSHA256 = strings.Repeat("de", 32)
		},
		"Go record with a package dir": func(e *domain.ProjectImageEnvironment) {
			*e = *goProjectImageEnvironmentFixture()
			e.SwiftPackageDir = "app"
		},
		"npm record with one Swift hash": func(e *domain.ProjectImageEnvironment) {
			*e = *projectImageEnvironmentFixture()
			e.PackageResolvedSHA256 = strings.Repeat("de", 32)
		},
		"npm record with a package dir": func(e *domain.ProjectImageEnvironment) {
			*e = *projectImageEnvironmentFixture()
			e.SwiftPackageDir = "app"
		},
	} {
		t.Run(name, func(t *testing.T) {
			environment := swiftProjectImageEnvironmentFixture()
			mutate(environment)
			if _, err := swiftProjectImage(t, environment); !errors.Is(err, domain.ErrProjectImageInvalid) {
				t.Fatalf("NewProjectImage() = %v, want ErrProjectImageInvalid", err)
			}
		})
	}
}

// A Swift record's ID and environment digest cover every Swift field, and
// neither can collide with an npm or Go record's: the toolchain selects the
// preimage version.
func TestSwiftProjectImageAddressCoversEverySwiftField(t *testing.T) {
	image, err := swiftProjectImage(t, swiftProjectImageEnvironmentFixture())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := image.Environment.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.ProjectImageEnvironment){
		"package dir":  func(e *domain.ProjectImageEnvironment) { e.SwiftPackageDir = "apps" },
		"root package": func(e *domain.ProjectImageEnvironment) { e.SwiftPackageDir = "" },
		"Package.swift": func(e *domain.ProjectImageEnvironment) {
			e.PackageSwiftSHA256 = strings.Repeat("ab", 32)
		},
		"Package.resolved": func(e *domain.ProjectImageEnvironment) {
			e.PackageResolvedSHA256 = strings.Repeat("ab", 32)
		},
		"preparation": func(e *domain.ProjectImageEnvironment) {
			e.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("ab", 32))
		},
	} {
		t.Run(name, func(t *testing.T) {
			altered := image
			altered.Environment = swiftProjectImageEnvironmentFixture()
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
		"npm": projectImageEnvironmentFixture(),
		"Go":  goProjectImageEnvironmentFixture(),
	} {
		otherDigest, err := other.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if otherDigest == digest {
			t.Fatalf("Swift and %s environments share a digest", name)
		}
	}
}

func TestSwiftProjectImageAdmissibleAt(t *testing.T) {
	const otherBase = "0123456789abcdef0123456789abcdef01234567"
	image, err := swiftProjectImage(t, swiftProjectImageEnvironmentFixture())
	if err != nil {
		t.Fatal(err)
	}
	current := image.Environment.PreparationDigest
	compatible := func() domain.ProjectImageBaseInputs {
		return domain.ProjectImageBaseInputs{
			CommitSHA:             otherBase,
			PackageSwiftSHA256:    image.Environment.PackageSwiftSHA256,
			PackageResolvedSHA256: image.Environment.PackageResolvedSHA256,
			RecipeDigest:          image.RecipeDigest,
		}
	}
	if err := image.AdmissibleAt(compatible(), current); err != nil {
		t.Fatalf("base with equal Package.swift and Package.resolved refused: %v", err)
	}
	if changed := image.Environment.ChangedManifests(compatible()); len(changed) != 0 {
		t.Fatalf("ChangedManifests() = %v at an equal base", changed)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*domain.ProjectImageBaseInputs)
		names  string
	}{
		{"changed Package.swift", func(b *domain.ProjectImageBaseInputs) {
			b.PackageSwiftSHA256 = strings.Repeat("ab", 32)
		}, "app/Package.swift at the base differs"},
		{"absent Package.swift", func(b *domain.ProjectImageBaseInputs) {
			b.PackageSwiftSHA256 = ""
		}, "app/Package.swift"},
		{"changed Package.resolved", func(b *domain.ProjectImageBaseInputs) {
			b.PackageResolvedSHA256 = strings.Repeat("ab", 32)
		}, "app/Package.resolved at the base differs"},
		{"absent Package.resolved", func(b *domain.ProjectImageBaseInputs) {
			b.PackageResolvedSHA256 = ""
		}, "app/Package.resolved"},
		{"mirrors file", func(b *domain.ProjectImageBaseInputs) {
			b.UnsupportedInputs = []string{"app/.swiftpm/configuration/mirrors.json"}
		}, "unsupported Swift input app/.swiftpm/configuration/mirrors.json"},
		{"inputs observed under the Go layout", func(b *domain.ProjectImageBaseInputs) {
			*b = domain.ProjectImageBaseInputs{
				CommitSHA:   otherBase,
				GoModSHA256: strings.Repeat("78", 32),
				GoSumSHA256: strings.Repeat("9a", 32),
			}
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

	bothChanged := compatible()
	bothChanged.PackageSwiftSHA256 = strings.Repeat("ab", 32)
	bothChanged.PackageResolvedSHA256 = strings.Repeat("ab", 32)
	if got := strings.Join(image.Environment.ChangedManifests(bothChanged), ","); got != "app/Package.swift,app/Package.resolved" {
		t.Fatalf("ChangedManifests() = %q, want both package manifests", got)
	}
}

// Adding the Swift fields leaves an npm and a Go record's bytes, ID, and
// environment digest as they were: both omit every Swift field.
func TestSwiftFieldsLeaveNPMAndGoRecordsUnchanged(t *testing.T) {
	for name, environment := range map[string]*domain.ProjectImageEnvironment{
		"npm": projectImageEnvironmentFixture(),
		"Go":  goProjectImageEnvironmentFixture(),
	} {
		image, err := goProjectImage(t, environment)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(image)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"swift_package_dir", "package_swift_sha256", "package_resolved_sha256"} {
			if strings.Contains(string(body), field) {
				t.Errorf("%s record encodes %s", name, field)
			}
		}
	}
	var image domain.ProjectImage
	if err := json.Unmarshal([]byte(preGoNPMRecord), &image); err != nil {
		t.Fatal(err)
	}
	if id, err := image.ComputeID(); err != nil || id != image.ID {
		t.Fatalf("stored npm record ComputeID() = %s, %v, want %s", id, err, image.ID)
	}
}
