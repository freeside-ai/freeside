package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
)

// selectionImage is a project image built at commit that bakes the npm
// manifests whose hashes repeat manifest, under image reference fill.
func selectionImage(t *testing.T, commit, manifest, fill string, edit func(*domain.ProjectImageInput)) domain.ProjectImage {
	t.Helper()
	return rebuildTestImage(t, func(input *domain.ProjectImageInput) {
		input.CommitSHA = commit
		input.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat(fill, 64))
		input.Environment = &domain.ProjectImageEnvironment{
			PackageJSONSHA256: strings.Repeat(manifest, 64), PackageLockSHA256: strings.Repeat(manifest, 64),
			PreparationDigest: projectimage.PreparationDigest(),
		}
		if edit != nil {
			edit(input)
		}
	})
}

// selectionBase is a base at commit holding the manifests selectionImage
// bakes for manifest. reads counts how often selection read it.
func selectionBase(commit, manifest string, reads *int) func(domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
	return func(domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
		*reads++
		return domain.ProjectImageBaseInputs{
			CommitSHA:         commit,
			PackageJSONSHA256: strings.Repeat(manifest, 64), PackageLockSHA256: strings.Repeat(manifest, 64),
		}, nil
	}
}

func TestSelectProjectImageOrder(t *testing.T) {
	t.Parallel()
	base := strings.Repeat("b", 40)
	pointed := selectionImage(t, strings.Repeat("a", 40), "2", "d", nil)
	// rebuilt keeps what the pointed-to image binds and bakes the base's
	// manifests, so it serves the base.
	rebuilt := selectionImage(t, base, "2", "e", nil)
	// stale bakes manifests the base no longer holds.
	stale := selectionImage(t, strings.Repeat("c", 40), "9", "f", nil)
	// other keeps nothing the pointed-to image binds.
	other := selectionImage(t, base, "2", "1", func(input *domain.ProjectImageInput) {
		input.RecipeDigest = domain.Digest("sha256:" + strings.Repeat("7", 64))
	})
	images := []domain.ProjectImage{stale, other, pointed, rebuilt}

	for name, tc := range map[string]struct {
		proved   domain.ImageRef
		manifest string
		images   []domain.ProjectImage
		want     domain.ImageRef
	}{
		"the proved image when it serves": {
			proved: rebuilt.ImageRef, manifest: "2", images: images, want: rebuilt.ImageRef,
		},
		"the proved image when it is the pointed-to image": {
			proved: pointed.ImageRef, manifest: "2", images: images, want: pointed.ImageRef,
		},
		"the pointed-to image over an unserving proved image": {
			proved: stale.ImageRef, manifest: "2", images: images, want: pointed.ImageRef,
		},
		"the pointed-to image over a proved image that keeps nothing it binds": {
			proved: other.ImageRef, manifest: "2", images: images, want: pointed.ImageRef,
		},
		"the pointed-to image when nothing is proved": {
			manifest: "2", images: images, want: pointed.ImageRef,
		},
		// An older base is served by an earlier write-once record: the base
		// holds the manifests stale baked, which the pointed-to image does
		// not.
		"the first recorded image that serves an older base": {
			proved: pointed.ImageRef, manifest: "9", images: images, want: stale.ImageRef,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			got, found, err := SelectProjectImage(ProjectImageSelection{
				Proved: tc.proved, Pointed: pointed, Images: tc.images, BaseSHA: base,
				BaseInputs:  selectionBase(base, tc.manifest, &reads),
				Preparation: projectimage.PreparationDigest(),
			})
			if err != nil || !found || got.ImageRef != tc.want {
				t.Fatalf("SelectProjectImage = (%s, %t, %v), want %s", got.ImageRef, found, err, tc.want)
			}
		})
	}

	t.Run("nothing when no image serves the base", func(t *testing.T) {
		t.Parallel()
		reads := 0
		got, found, err := SelectProjectImage(ProjectImageSelection{
			Proved: pointed.ImageRef, Pointed: pointed, Images: images, BaseSHA: base,
			BaseInputs:  selectionBase(base, "5", &reads),
			Preparation: projectimage.PreparationDigest(),
		})
		if err != nil || found {
			t.Fatalf("SelectProjectImage = (%s, %t, %v), want nothing", got.ImageRef, found, err)
		}
		if reads != 1 {
			t.Fatalf("the base was read %d times, want once for the one input layout", reads)
		}
	})
}

// The pointed-to image and every record are decoded state, so each is
// re-gated before it is chosen, even at its own build commit, where
// AdmissibleAt alone would take it from the record.
func TestSelectProjectImageRegatesDecodedRecords(t *testing.T) {
	t.Parallel()
	base := strings.Repeat("b", 40)
	for name, edit := range map[string]func(*domain.ProjectImageInput){
		"another preparation command": func(input *domain.ProjectImageInput) {
			input.PreparationCommand = []string{"/bin/sh", "-c", "true"}
		},
		"another binary's preparation sources": func(input *domain.ProjectImageInput) {
			input.Environment.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("6", 64))
		},
		"no recorded environment": func(input *domain.ProjectImageInput) { input.Environment = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pointed := selectionImage(t, base, "2", "d", edit)
			reads := 0
			got, found, err := SelectProjectImage(ProjectImageSelection{
				Proved: pointed.ImageRef, Pointed: pointed, Images: []domain.ProjectImage{pointed},
				BaseSHA: base, BaseInputs: selectionBase(base, "2", &reads),
				Preparation: projectimage.PreparationDigest(),
			})
			if err != nil || found {
				t.Fatalf("SelectProjectImage = (%s, %t, %v), want the record refused", got.ImageRef, found, err)
			}
		})
	}
}

func TestSelectProjectImageReadsTheBaseOnlyWhenItMust(t *testing.T) {
	t.Parallel()
	base := strings.Repeat("b", 40)
	pointed := selectionImage(t, base, "2", "d", nil)
	reads := 0
	got, found, err := SelectProjectImage(ProjectImageSelection{
		Proved: pointed.ImageRef, Pointed: pointed, Images: []domain.ProjectImage{pointed},
		BaseSHA: base, BaseInputs: selectionBase(base, "2", &reads),
		Preparation: projectimage.PreparationDigest(),
	})
	if err != nil || !found || got.ID != pointed.ID {
		t.Fatalf("SelectProjectImage = (%s, %t, %v), want the pointed-to image", got.ImageRef, found, err)
	}
	if reads != 0 {
		t.Fatalf("the base was read %d times for an image built at it", reads)
	}

	elsewhere := selectionImage(t, strings.Repeat("a", 40), "2", "d", nil)
	failure := errors.New("fetch failed")
	if _, _, err := SelectProjectImage(ProjectImageSelection{
		Pointed: elsewhere, BaseSHA: base, Preparation: projectimage.PreparationDigest(),
		BaseInputs: func(domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
			return domain.ProjectImageBaseInputs{}, failure
		},
	}); !errors.Is(err, failure) {
		t.Fatalf("a failed base read = %v, want it returned", err)
	}
	reads = 0
	if _, found, err := SelectProjectImage(ProjectImageSelection{
		Pointed: elsewhere, BaseSHA: base, Preparation: projectimage.PreparationDigest(),
		BaseInputs: selectionBase(strings.Repeat("c", 40), "2", &reads),
	}); err == nil || found {
		t.Fatalf("inputs read at another commit = (%t, %v), want an error", found, err)
	}
}
