package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

const (
	// projectImageEncodingVersion addresses a record without an environment.
	// It must never change: every image recorded before environment evidence
	// existed validates against this preimage.
	projectImageEncodingVersion = "freeside.project-image/v1"
	// projectImageEnvironmentEncodingVersion addresses a record that carries
	// an environment, which the preimage then includes.
	projectImageEnvironmentEncodingVersion = "freeside.project-image/v2"
	projectImageEnvironmentVersion         = "freeside.project-image-environment/v1"
)

var (
	// ErrProjectImageInvalid marks a malformed project-image provenance field.
	ErrProjectImageInvalid = errors.New("project image provenance is invalid")
	// ErrProjectImageInconsistent marks a record whose ID does not address its
	// provenance and produced image.
	ErrProjectImageInconsistent = errors.New("project image identity does not match its content")
	// ErrProjectImageIncompatible marks a run base the image's recorded
	// environment does not cover. It is a verdict about one (image, base)
	// pair, never a defect in the record.
	ErrProjectImageIncompatible = errors.New("project image is not compatible with the run base")
)

var (
	projectRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	projectCommitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	projectSHA256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ProjectImage is the immutable result of building one managed repository's
// runtime image. It binds the forge's stable repository identity, exact source
// commit, trusted recipe bytes by digest, workspace-preparation command, base
// image, and produced image. A run can therefore use ImageRef without losing
// which inputs made those bytes admissible.
//
// CommitSHA is the commit whose tree supplied the build inputs, not the base of
// any run that uses the image: a run records its own base in its
// ExecutionAdmission, and AdmissibleAt decides whether this image may serve
// that base. Environment is nil on a record built before environment evidence
// existed; such a record is admissible only at CommitSHA.
//
// PreparationCommand is executed by the project-image-aware verification room
// before each recipe argv in its fresh workspace. It is image-owned setup, not
// a recipe rewrite: the selected npm image uses the fixed helper baked at
// /usr/local/bin/freeside-project-prepare to hydrate dependencies from the
// image's cache before the trusted recipe argv is executed verbatim.
type ProjectImage struct {
	ID                 Digest   `json:"id"`
	Repository         string   `json:"repository"`
	RepositoryID       int64    `json:"repository_id"`
	CommitSHA          string   `json:"commit_sha"`
	RecipeDigest       Digest   `json:"recipe_digest"`
	PreparationCommand []string `json:"preparation_command"`
	BaseImageRef       ImageRef `json:"base_image_ref"`
	ImageRef           ImageRef `json:"image_ref"`
	// Environment is the supported input set the image baked, or nil.
	Environment *ProjectImageEnvironment `json:"environment"`
}

// ProjectImageEnvironment records the inputs that define a project image's
// runtime environment beyond the fields ProjectImage already binds
// (RecipeDigest and BaseImageRef). A base whose inputs equal these gets the
// same environment from the image as the build commit did.
type ProjectImageEnvironment struct {
	// PackageJSONSHA256 and PackageLockSHA256 are the lowercase hex SHA-256
	// of the exact manifest bytes baked as the image's dependency seed.
	PackageJSONSHA256 string `json:"package_json_sha256"`
	PackageLockSHA256 string `json:"package_lock_sha256"`
	// PreparationDigest addresses the builder's fixed toolchain and
	// preparation sources at build time.
	PreparationDigest Digest `json:"preparation_digest"`
}

// ProjectImageBaseInputs is what one run base holds for each input a project
// image bakes, observed from that exact commit's tree. A hash or digest is
// empty when the base holds no file at the path.
type ProjectImageBaseInputs struct {
	CommitSHA         string
	PackageJSONSHA256 string
	PackageLockSHA256 string
	// RecipeDigest addresses the recipe the base declares in its own tree. It
	// is empty for a repository whose recipe is supplied outside the tree,
	// which declares nothing an image's baked recipe could contradict.
	RecipeDigest Digest
	// UnsupportedInputs names the installation-configuration files present at
	// the base that no project image supports.
	UnsupportedInputs []string
}

// ProjectImageInput carries caller-supplied build facts. ID is derived from
// them and can never be asserted by a caller.
type ProjectImageInput struct {
	Repository         string
	RepositoryID       int64
	CommitSHA          string
	RecipeDigest       Digest
	PreparationCommand []string
	BaseImageRef       ImageRef
	ImageRef           ImageRef
	Environment        *ProjectImageEnvironment
}

type canonicalProjectImage struct {
	Version            string   `json:"version"`
	Repository         string   `json:"repository"`
	RepositoryID       int64    `json:"repository_id"`
	CommitSHA          string   `json:"commit_sha"`
	RecipeDigest       Digest   `json:"recipe_digest"`
	PreparationCommand []string `json:"preparation_command"`
	BaseImageRef       ImageRef `json:"base_image_ref"`
	ImageRef           ImageRef `json:"image_ref"`
}

type canonicalProjectImageWithEnvironment struct {
	canonicalProjectImage
	Environment ProjectImageEnvironment `json:"environment"`
}

type canonicalProjectImageEnvironment struct {
	Version string `json:"version"`
	ProjectImageEnvironment
}

// NewProjectImage builds a detached, content-addressed project-image record.
func NewProjectImage(in ProjectImageInput) (ProjectImage, error) {
	image := ProjectImage{
		Repository:         in.Repository,
		RepositoryID:       in.RepositoryID,
		CommitSHA:          in.CommitSHA,
		RecipeDigest:       in.RecipeDigest,
		PreparationCommand: append([]string{}, in.PreparationCommand...),
		BaseImageRef:       in.BaseImageRef,
		ImageRef:           in.ImageRef,
	}
	if in.Environment != nil {
		environment := *in.Environment
		image.Environment = &environment
	}
	id, err := image.ComputeID()
	if err != nil {
		return ProjectImage{}, err
	}
	image.ID = id
	if err := image.Validate(); err != nil {
		return ProjectImage{}, err
	}
	return image, nil
}

// ComputeID returns the versioned content address of every project-image
// provenance fact. It deliberately includes ImageRef: Apple container build
// metadata may make two builds from the same pinned inputs produce distinct
// digests, and each produced artifact needs its own truthful record.
//
// A record without an environment keeps the v1 preimage so its existing ID
// still validates. A record with one uses the v2 preimage, which includes it,
// so the environment cannot be added to, removed from, or altered in a stored
// record without changing the ID.
func (p ProjectImage) ComputeID() (Digest, error) {
	canonical := canonicalProjectImage{
		Version:            projectImageEncodingVersion,
		Repository:         p.Repository,
		RepositoryID:       p.RepositoryID,
		CommitSHA:          p.CommitSHA,
		RecipeDigest:       p.RecipeDigest,
		PreparationCommand: p.PreparationCommand,
		BaseImageRef:       p.BaseImageRef,
		ImageRef:           p.ImageRef,
	}
	var preimage any = canonical
	if p.Environment != nil {
		canonical.Version = projectImageEnvironmentEncodingVersion
		preimage = canonicalProjectImageWithEnvironment{
			canonicalProjectImage: canonical, Environment: *p.Environment,
		}
	}
	body, err := json.Marshal(preimage)
	if err != nil {
		return "", fmt.Errorf("project image id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Digest returns the versioned content address of the environment alone, the
// lookup value a store keeps beside the record body.
func (e ProjectImageEnvironment) Digest() (Digest, error) {
	body, err := json.Marshal(canonicalProjectImageEnvironment{
		Version: projectImageEnvironmentVersion, ProjectImageEnvironment: e,
	})
	if err != nil {
		return "", fmt.Errorf("project image environment digest: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

func (e ProjectImageEnvironment) validate() error {
	if !projectSHA256Pattern.MatchString(e.PackageJSONSHA256) {
		return fmt.Errorf("project image environment package_json_sha256 %q: %w",
			e.PackageJSONSHA256, ErrProjectImageInvalid)
	}
	if !projectSHA256Pattern.MatchString(e.PackageLockSHA256) {
		return fmt.Errorf("project image environment package_lock_sha256 %q: %w",
			e.PackageLockSHA256, ErrProjectImageInvalid)
	}
	if !contentaddr.Valid(string(e.PreparationDigest)) {
		return fmt.Errorf("project image environment preparation_digest %q: %w",
			e.PreparationDigest, ErrProjectImageInvalid)
	}
	return nil
}

// AdmissibleAt reports whether the image may serve a run whose base is the
// observed commit, as nil or an ErrProjectImageIncompatible naming the first
// input that rules it out. currentPreparation is the running binary's
// preparation digest.
//
// The image's own build commit is always admissible, and for a record without
// an environment it is the only admissible base. Any other base is admissible
// only when every recorded input is unchanged there: both dependency
// manifests, the absence of unsupported installation configuration, and the
// preparation implementation; and when the base declares no recipe that
// contradicts the baked one.
//
// The recipe is the one input a base need not hold. An image runs the recipe
// it baked, which the caller checks against current approval, so a base that
// declares none (a recipe supplied outside the tree, or removed from it)
// leaves that recipe the only statement of how to verify. A base that declares
// a different recipe says verification changed, and the image would run the
// stale one.
//
// This decides environment compatibility only. Repository identity, the
// preparation argv, and recipe approval are separate checks the caller still
// owns, and a nil result never substitutes for verifying the run's own source.
// No caller stores the verdict: each boundary re-derives it from the immutable
// record and the base tree.
func (p ProjectImage) AdmissibleAt(base ProjectImageBaseInputs, currentPreparation Digest) error {
	if base.CommitSHA == p.CommitSHA {
		return nil
	}
	// The cause leads and the identities trail: an operator-facing surface
	// that shows a bounded excerpt of this error still shows why.
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s (image %s built at %s, run base %s)",
			ErrProjectImageIncompatible, fmt.Sprintf(format, args...),
			p.ImageRef, p.CommitSHA, base.CommitSHA)
	}
	if p.Environment == nil {
		return refuse("the record predates environment evidence and is usable only at its " +
			"build commit; rebuild the image to reuse it across source-only commits")
	}
	if len(base.UnsupportedInputs) != 0 {
		return refuse("the base contains unsupported npm input %s",
			strings.Join(base.UnsupportedInputs, ", "))
	}
	if base.PackageJSONSHA256 != p.Environment.PackageJSONSHA256 {
		return refuse("package.json at the base differs from the baked manifest")
	}
	if base.PackageLockSHA256 != p.Environment.PackageLockSHA256 {
		return refuse("package-lock.json at the base differs from the baked manifest")
	}
	if base.RecipeDigest != "" && base.RecipeDigest != p.RecipeDigest {
		return refuse("the base declares a different verification recipe (%s) than the image baked (%s)",
			base.RecipeDigest, p.RecipeDigest)
	}
	if currentPreparation != p.Environment.PreparationDigest {
		return refuse("the image was built with different toolchain and preparation sources; "+
			"rebuild the image to reuse it (image sources %s, this binary's %q)",
			p.Environment.PreparationDigest, currentPreparation)
	}
	return nil
}

// Validate is the reconstruction backstop for a project-image build result.
func (p ProjectImage) Validate() error {
	if !projectRepositoryPattern.MatchString(p.Repository) ||
		strings.Contains(p.Repository, "..") {
		return fmt.Errorf("project image repository %q: %w", p.Repository, ErrProjectImageInvalid)
	}
	if p.RepositoryID <= 0 {
		return fmt.Errorf("project image repository_id %d: %w", p.RepositoryID, ErrNonPositive)
	}
	if !projectCommitPattern.MatchString(p.CommitSHA) {
		return fmt.Errorf("project image commit_sha %q: %w", p.CommitSHA, ErrProjectImageInvalid)
	}
	if !contentaddr.Valid(string(p.RecipeDigest)) {
		return fmt.Errorf("project image recipe_digest %q: %w",
			p.RecipeDigest, ErrProjectImageInvalid)
	}
	if len(p.PreparationCommand) == 0 || p.PreparationCommand[0] == "" {
		return fmt.Errorf("project image preparation_command: %w", ErrEmptyField)
	}
	for index, token := range p.PreparationCommand {
		if strings.ContainsRune(token, 0) {
			return fmt.Errorf("project image preparation_command[%d] contains NUL: %w",
				index, ErrProjectImageInvalid)
		}
	}
	if err := p.BaseImageRef.Validate(); err != nil {
		return fmt.Errorf("project image base: %w", err)
	}
	if err := p.ImageRef.Validate(); err != nil {
		return fmt.Errorf("project image result: %w", err)
	}
	if p.Environment != nil {
		if err := p.Environment.validate(); err != nil {
			return err
		}
	}
	if p.ID == "" {
		return fmt.Errorf("project image id: %w", ErrEmptyID)
	}
	computed, err := p.ComputeID()
	if err != nil {
		return err
	}
	if p.ID != computed {
		return fmt.Errorf("project image %s, content resolves to %s: %w",
			p.ID, computed, ErrProjectImageInconsistent)
	}
	return nil
}
