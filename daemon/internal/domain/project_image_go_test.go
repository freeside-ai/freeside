package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func goProjectImageEnvironmentFixture() *domain.ProjectImageEnvironment {
	return &domain.ProjectImageEnvironment{
		Toolchain:         domain.ProjectToolchainGo,
		GoModuleDir:       "daemon",
		GoModSHA256:       strings.Repeat("78", 32),
		GoSumSHA256:       strings.Repeat("9a", 32),
		PreparationDigest: domain.Digest("sha256:" + strings.Repeat("56", 32)),
	}
}

func goProjectImage(t *testing.T, environment *domain.ProjectImageEnvironment) (domain.ProjectImage, error) {
	t.Helper()
	legacy := projectImageFixture(t)
	return domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeside-ai/freeside", RepositoryID: legacy.RepositoryID,
		CommitSHA: legacy.CommitSHA, RecipeDigest: legacy.RecipeDigest,
		PreparationCommand: legacy.PreparationCommand,
		BaseImageRef:       legacy.BaseImageRef, ImageRef: legacy.ImageRef,
		Environment: environment,
	})
}

func TestProjectImageInputLayoutValidate(t *testing.T) {
	for _, toolchain := range domain.AllProjectToolchains {
		if err := (domain.ProjectImageInputLayout{Toolchain: toolchain}).Validate(); err != nil {
			t.Errorf("root %s layout: %v", toolchain, err)
		}
	}
	if err := (domain.ProjectImageInputLayout{
		Toolchain: domain.ProjectToolchainGo, GoModuleDir: "daemon/cmd",
	}).Validate(); err != nil {
		t.Errorf("nested Go module layout: %v", err)
	}
	for name, layout := range map[string]domain.ProjectImageInputLayout{
		"empty toolchain":         {},
		"unknown toolchain":       {Toolchain: "uv"},
		"npm with a module dir":   {Toolchain: domain.ProjectToolchainNPM, GoModuleDir: "daemon"},
		"Go with an invalid path": {Toolchain: domain.ProjectToolchainGo, GoModuleDir: "../daemon"},
	} {
		if err := layout.Validate(); !errors.Is(err, domain.ErrProjectImageInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrProjectImageInvalid", name, err)
		}
	}
}

func TestValidProjectGoModuleDir(t *testing.T) {
	for _, dir := range []string{"", "daemon", "services/api", "a_b/c-d/e.f", "v2"} {
		if !domain.ValidProjectGoModuleDir(dir) {
			t.Errorf("ValidProjectGoModuleDir(%q) = false, want true", dir)
		}
	}
	for _, dir := range []string{
		".", "..", "./daemon", "../daemon", "daemon/..", "daemon/../x", "/daemon",
		"daemon/", "a//b", ".hidden", "-flag", "has space", "quote'd", "a\\b",
		strings.Repeat("a", 256),
	} {
		if domain.ValidProjectGoModuleDir(dir) {
			t.Errorf("ValidProjectGoModuleDir(%q) = true, want false", dir)
		}
	}
}

func TestGoProjectImageEnvironmentValidation(t *testing.T) {
	image, err := goProjectImage(t, goProjectImageEnvironmentFixture())
	if err != nil {
		t.Fatalf("Go record refused: %v", err)
	}
	if got := image.InputLayout(); got != (domain.ProjectImageInputLayout{
		Toolchain: domain.ProjectToolchainGo, GoModuleDir: "daemon",
	}) {
		t.Fatalf("InputLayout() = %+v", got)
	}
	root := goProjectImageEnvironmentFixture()
	root.GoModuleDir = ""
	if _, err := goProjectImage(t, root); err != nil {
		t.Fatalf("Go record with its module at the root refused: %v", err)
	}

	for name, mutate := range map[string]func(*domain.ProjectImageEnvironment){
		"Go record with npm hashes": func(e *domain.ProjectImageEnvironment) {
			e.PackageJSONSHA256 = strings.Repeat("12", 32)
			e.PackageLockSHA256 = strings.Repeat("34", 32)
		},
		"Go record with one npm hash": func(e *domain.ProjectImageEnvironment) {
			e.PackageLockSHA256 = strings.Repeat("34", 32)
		},
		"Go record missing go.sum": func(e *domain.ProjectImageEnvironment) { e.GoSumSHA256 = "" },
		"Go record missing go.mod": func(e *domain.ProjectImageEnvironment) { e.GoModSHA256 = "" },
		"malformed go.mod hash":    func(e *domain.ProjectImageEnvironment) { e.GoModSHA256 = "1234" },
		"escaping module dir":      func(e *domain.ProjectImageEnvironment) { e.GoModuleDir = "../daemon" },
		"absolute module dir":      func(e *domain.ProjectImageEnvironment) { e.GoModuleDir = "/daemon" },
		"explicit npm toolchain": func(e *domain.ProjectImageEnvironment) {
			*e = *projectImageEnvironmentFixture()
			e.Toolchain = domain.ProjectToolchainNPM
		},
		"unknown toolchain": func(e *domain.ProjectImageEnvironment) { e.Toolchain = "uv" },
		"npm record with Go hashes": func(e *domain.ProjectImageEnvironment) {
			*e = *projectImageEnvironmentFixture()
			e.GoModSHA256 = strings.Repeat("78", 32)
			e.GoSumSHA256 = strings.Repeat("9a", 32)
		},
		"npm record with a module dir": func(e *domain.ProjectImageEnvironment) {
			*e = *projectImageEnvironmentFixture()
			e.GoModuleDir = "daemon"
		},
	} {
		t.Run(name, func(t *testing.T) {
			environment := goProjectImageEnvironmentFixture()
			mutate(environment)
			if _, err := goProjectImage(t, environment); !errors.Is(err, domain.ErrProjectImageInvalid) {
				t.Fatalf("NewProjectImage() = %v, want ErrProjectImageInvalid", err)
			}
		})
	}
}

// A Go record's ID and environment digest cover every Go field, and neither
// can collide with an npm record's: the toolchain selects the preimage
// version.
func TestGoProjectImageAddressCoversEveryGoField(t *testing.T) {
	image, err := goProjectImage(t, goProjectImageEnvironmentFixture())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := image.Environment.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.ProjectImageEnvironment){
		"module dir": func(e *domain.ProjectImageEnvironment) { e.GoModuleDir = "services" },
		"root module": func(e *domain.ProjectImageEnvironment) {
			e.GoModuleDir = ""
		},
		"go.mod": func(e *domain.ProjectImageEnvironment) { e.GoModSHA256 = strings.Repeat("ab", 32) },
		"go.sum": func(e *domain.ProjectImageEnvironment) { e.GoSumSHA256 = strings.Repeat("ab", 32) },
		"preparation": func(e *domain.ProjectImageEnvironment) {
			e.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("ab", 32))
		},
	} {
		t.Run(name, func(t *testing.T) {
			altered := image
			altered.Environment = goProjectImageEnvironmentFixture()
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
	npmDigest, err := projectImageEnvironmentFixture().Digest()
	if err != nil {
		t.Fatal(err)
	}
	if npmDigest == digest {
		t.Fatal("Go and npm environments share a digest")
	}
}

func TestGoProjectImageAdmissibleAt(t *testing.T) {
	const otherBase = "0123456789abcdef0123456789abcdef01234567"
	image, err := goProjectImage(t, goProjectImageEnvironmentFixture())
	if err != nil {
		t.Fatal(err)
	}
	current := image.Environment.PreparationDigest
	compatible := func() domain.ProjectImageBaseInputs {
		return domain.ProjectImageBaseInputs{
			CommitSHA:    otherBase,
			GoModSHA256:  image.Environment.GoModSHA256,
			GoSumSHA256:  image.Environment.GoSumSHA256,
			RecipeDigest: image.RecipeDigest,
		}
	}
	if err := image.AdmissibleAt(compatible(), current); err != nil {
		t.Fatalf("base with equal go.mod and go.sum refused: %v", err)
	}
	if changed := image.Environment.ChangedManifests(compatible()); len(changed) != 0 {
		t.Fatalf("ChangedManifests() = %v at an equal base", changed)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*domain.ProjectImageBaseInputs)
		names  string
	}{
		{"changed go.mod", func(b *domain.ProjectImageBaseInputs) {
			b.GoModSHA256 = strings.Repeat("ab", 32)
		}, "daemon/go.mod at the base differs"},
		{"absent go.mod", func(b *domain.ProjectImageBaseInputs) { b.GoModSHA256 = "" }, "daemon/go.mod"},
		{"changed go.sum", func(b *domain.ProjectImageBaseInputs) {
			b.GoSumSHA256 = strings.Repeat("ab", 32)
		}, "daemon/go.sum at the base differs"},
		{"absent go.sum", func(b *domain.ProjectImageBaseInputs) { b.GoSumSHA256 = "" }, "daemon/go.sum"},
		{"vendor directory", func(b *domain.ProjectImageBaseInputs) {
			b.UnsupportedInputs = []string{"daemon/vendor"}
		}, "unsupported Go input daemon/vendor"},
		{"go.work above the module", func(b *domain.ProjectImageBaseInputs) {
			b.UnsupportedInputs = []string{"go.work"}
		}, "unsupported Go input go.work"},
		{"inputs observed under the npm layout", func(b *domain.ProjectImageBaseInputs) {
			*b = domain.ProjectImageBaseInputs{
				CommitSHA:         otherBase,
				PackageJSONSHA256: strings.Repeat("12", 32),
				PackageLockSHA256: strings.Repeat("34", 32),
			}
		}, "daemon/go.mod"},
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
	bothChanged.GoModSHA256 = strings.Repeat("ab", 32)
	bothChanged.GoSumSHA256 = strings.Repeat("ab", 32)
	if got := strings.Join(image.Environment.ChangedManifests(bothChanged), ","); got != "daemon/go.mod,daemon/go.sum" {
		t.Fatalf("ChangedManifests() = %q, want both module manifests", got)
	}
}

// preGoNPMRecord is project_image_environment.golden as it stood before the
// Go toolchain path existed. Stored npm rows hold exactly these bytes.
const preGoNPMRecord = `{
  "id": "sha256:75bb1352aca573be4b24605229cb67b2825f01b051d889e8ccdab830628bc5db",
  "repository": "freeasinbird/gh-imgup",
  "repository_id": 1278475858,
  "commit_sha": "6ab4e3dff2be53f74bde9b8b3150290775152f9f",
  "recipe_digest": "sha256:efefefefefefefefefefefefefefefefefefefefefefefefefefefefefefefef",
  "preparation_command": [
    "/usr/local/bin/freeside-project-prepare"
  ],
  "base_image_ref": "ghcr.io/freeside-ai/agent-claude@sha256:abababababababababababababababababababababababababababababababab",
  "image_ref": "127.0.0.1:5100/freeside-project-freeasinbird-gh-imgup@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
  "environment": {
    "package_json_sha256": "1212121212121212121212121212121212121212121212121212121212121212",
    "package_lock_sha256": "3434343434343434343434343434343434343434343434343434343434343434",
    "preparation_digest": "sha256:5656565656565656565656565656565656565656565656565656565656565656"
  }
}`

// A stored npm record decodes, validates, and keeps its ID and environment
// digest, so neither the record bodies nor the environment_digest column
// needs a rewrite, and it is admitted exactly as before.
func TestPreGoNPMRecordKeepsItsAddressesAndVerdicts(t *testing.T) {
	var image domain.ProjectImage
	if err := json.Unmarshal([]byte(preGoNPMRecord), &image); err != nil {
		t.Fatal(err)
	}
	if err := image.Validate(); err != nil {
		t.Fatalf("stored npm record no longer validates: %v", err)
	}
	if id, err := image.ComputeID(); err != nil || id != image.ID {
		t.Fatalf("ComputeID() = %s, %v, want the stored %s", id, err, image.ID)
	}
	// The environment digest the store recorded beside the body, computed
	// from the pre-Go canonical bytes.
	const storedEnvironmentDigest = "sha256:e09c1e7f645b53112400b9180b7ded1c3ff928ea55521c510a2c16af1f6e5db7"
	if digest, err := image.Environment.Digest(); err != nil || digest != storedEnvironmentDigest {
		t.Fatalf("environment Digest() = %s, %v, want %s", digest, err, storedEnvironmentDigest)
	}
	if image.InputLayout() != (domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM}) {
		t.Fatalf("stored record reads as layout %+v, want npm at the root", image.InputLayout())
	}
	reencoded, err := json.MarshalIndent(image, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != preGoNPMRecord {
		t.Fatalf("stored record re-encodes differently:\n%s", reencoded)
	}
	base := domain.ProjectImageBaseInputs{
		CommitSHA:         "0123456789abcdef0123456789abcdef01234567",
		PackageJSONSHA256: image.Environment.PackageJSONSHA256,
		PackageLockSHA256: image.Environment.PackageLockSHA256,
		RecipeDigest:      image.RecipeDigest,
	}
	if err := image.AdmissibleAt(base, image.Environment.PreparationDigest); err != nil {
		t.Fatalf("stored npm record refused at a compatible base: %v", err)
	}
	base.PackageLockSHA256 = strings.Repeat("ab", 32)
	if err := image.AdmissibleAt(base, image.Environment.PreparationDigest); err == nil ||
		!strings.Contains(err.Error(), "package-lock.json at the base differs from the baked manifest") {
		t.Fatalf("stored npm record at a changed lockfile = %v", err)
	}
}
