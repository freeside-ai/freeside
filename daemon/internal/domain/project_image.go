package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

const (
	// projectImageEncodingVersion addresses a record without an environment.
	// It must never change: every image recorded before environment evidence
	// existed validates against this preimage.
	projectImageEncodingVersion = "freeside.project-image/v1"
	// projectImageEnvironmentEncodingVersion addresses an npm record that
	// carries an environment, which the preimage then includes.
	projectImageEnvironmentEncodingVersion = "freeside.project-image/v2"
	projectImageEnvironmentVersion         = "freeside.project-image-environment/v1"
	// projectImageGoEncodingVersion and projectImageGoEnvironmentVersion
	// address a Go record. npm records keep the v2 preimage and v1 environment
	// digest byte for byte, so no stored ID or environment digest changes; a
	// Go record cannot share an address with any npm record.
	projectImageGoEncodingVersion    = "freeside.project-image/v3"
	projectImageGoEnvironmentVersion = "freeside.project-image-environment/v2"
)

// ProjectToolchain names the toolchain path a project image bakes (plan §5.7,
// Toolchains).
type ProjectToolchain string

const (
	// ProjectToolchainNPM bakes a pinned Node distribution and the npm ci
	// closure of the root package.json and package-lock.json.
	ProjectToolchainNPM ProjectToolchain = "npm"
	// ProjectToolchainGo bakes a pinned Go distribution, golangci-lint, and
	// the module cache one module's go.mod and go.sum pin.
	ProjectToolchainGo ProjectToolchain = "go"
)

// AllProjectToolchains lists every valid ProjectToolchain.
var AllProjectToolchains = []ProjectToolchain{ProjectToolchainNPM, ProjectToolchainGo}

func (t ProjectToolchain) valid() bool {
	switch t {
	case ProjectToolchainNPM, ProjectToolchainGo:
		return true
	default:
		return false
	}
}

// ProjectImageInputLayout says where a project image's environment inputs
// live in a tree: the toolchain and, for Go, the module directory relative
// to the repository root ("" is the root).
type ProjectImageInputLayout struct {
	Toolchain   ProjectToolchain
	GoModuleDir string
}

// Validate rejects a layout no project image can record.
func (l ProjectImageInputLayout) Validate() error {
	if !l.Toolchain.valid() {
		return fmt.Errorf("project image toolchain %q: %w", l.Toolchain, ErrProjectImageInvalid)
	}
	if !ValidProjectGoModuleDir(l.GoModuleDir) ||
		(l.Toolchain == ProjectToolchainNPM && l.GoModuleDir != "") {
		return fmt.Errorf("project image go_module_dir %q: %w", l.GoModuleDir, ErrProjectImageInvalid)
	}
	return nil
}

// projectGoModuleDirSegment admits no leading dot, so neither "." nor ".."
// nor a hidden directory can be a segment, and no character a shell or a
// Containerfile would interpret.
var projectGoModuleDirSegment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// ValidProjectGoModuleDir reports whether dir can name a Go image's module
// directory: empty for the repository root, or a relative slash-separated
// path whose segments are plain names. It never admits an absolute path, a
// ".." segment, or a non-canonical spelling, so one directory has one name.
func ValidProjectGoModuleDir(dir string) bool {
	if dir == "" {
		return true
	}
	if len(dir) > 255 {
		return false
	}
	for _, segment := range strings.Split(dir, "/") {
		if !projectGoModuleDirSegment.MatchString(segment) {
			return false
		}
	}
	return true
}

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
//
// Every field a toolchain does not use is omitted, not rendered null: an npm
// record must keep the exact JSON it had before Go fields existed, because
// its ID and environment digest hash those bytes.
type ProjectImageEnvironment struct {
	// Toolchain is empty for npm, which every record built before the Go
	// path carries, and "go" for a Go image; an explicit "npm" is invalid so
	// one environment has one encoding. ResolvedToolchain reads it.
	Toolchain ProjectToolchain `json:"toolchain,omitempty"`
	// GoModuleDir is a Go image's module directory relative to the
	// repository root, empty for the root.
	GoModuleDir string `json:"go_module_dir,omitempty"`
	// PackageJSONSHA256 and PackageLockSHA256 are the lowercase hex SHA-256
	// of the exact npm manifest bytes baked as the image's dependency seed.
	PackageJSONSHA256 string `json:"package_json_sha256,omitempty"`
	PackageLockSHA256 string `json:"package_lock_sha256,omitempty"`
	// GoModSHA256 and GoSumSHA256 are the same for a Go image's module.
	GoModSHA256 string `json:"go_mod_sha256,omitempty"`
	GoSumSHA256 string `json:"go_sum_sha256,omitempty"`
	// PreparationDigest addresses the builder's fixed toolchain and
	// preparation sources at build time.
	PreparationDigest Digest `json:"preparation_digest"`
}

// ResolvedToolchain is the toolchain the environment baked: npm when the
// record names none.
func (e ProjectImageEnvironment) ResolvedToolchain() ProjectToolchain {
	if e.Toolchain == "" {
		return ProjectToolchainNPM
	}
	return e.Toolchain
}

// ProjectImageBaseInputs is what one run base holds for each input a project
// image bakes, observed from that exact commit's tree under one image's input
// layout. A hash or digest is empty when the base holds no file at the path,
// and the hashes of the other toolchain are always empty.
type ProjectImageBaseInputs struct {
	CommitSHA         string
	PackageJSONSHA256 string
	PackageLockSHA256 string
	// GoModSHA256 and GoSumSHA256 hash the layout's module go.mod and go.sum.
	GoModSHA256 string
	GoSumSHA256 string
	// RecipeDigest addresses the recipe the base declares in its own tree. It
	// is empty for a repository whose recipe is supplied outside the tree,
	// which declares nothing an image's baked recipe could contradict.
	RecipeDigest Digest
	// UnsupportedInputs names the inputs present at the base that no project
	// image of the layout's toolchain supports: npm's lock and configuration
	// files, or a Go module's vendor directory, a go.work file, or a go.mod
	// replace whose target is a local path.
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
// still validates. A record with one uses the v2 preimage for npm and v3 for
// Go, which include it, so the environment cannot be added to, removed from,
// or altered in a stored record without changing the ID.
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
		if p.Environment.ResolvedToolchain() == ProjectToolchainGo {
			canonical.Version = projectImageGoEncodingVersion
		}
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
	version := projectImageEnvironmentVersion
	if e.ResolvedToolchain() == ProjectToolchainGo {
		version = projectImageGoEnvironmentVersion
	}
	body, err := json.Marshal(canonicalProjectImageEnvironment{
		Version: version, ProjectImageEnvironment: e,
	})
	if err != nil {
		return "", fmt.Errorf("project image environment digest: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

func (e ProjectImageEnvironment) validate() error {
	goImage := false
	switch e.Toolchain {
	case "":
		if e.GoModuleDir != "" {
			return fmt.Errorf("project image npm environment names go_module_dir %q: %w",
				e.GoModuleDir, ErrProjectImageInvalid)
		}
	case ProjectToolchainGo:
		goImage = true
		if !ValidProjectGoModuleDir(e.GoModuleDir) {
			return fmt.Errorf("project image environment go_module_dir %q: %w",
				e.GoModuleDir, ErrProjectImageInvalid)
		}
	default:
		return fmt.Errorf("project image environment toolchain %q: %w",
			e.Toolchain, ErrProjectImageInvalid)
	}
	// Exactly the toolchain's own pair: the other pair's presence would make
	// the record claim inputs it never baked.
	for _, hash := range []struct {
		name, value string
		required    bool
	}{
		{"package_json_sha256", e.PackageJSONSHA256, !goImage},
		{"package_lock_sha256", e.PackageLockSHA256, !goImage},
		{"go_mod_sha256", e.GoModSHA256, goImage},
		{"go_sum_sha256", e.GoSumSHA256, goImage},
	} {
		if hash.required && !projectSHA256Pattern.MatchString(hash.value) {
			return fmt.Errorf("project image environment %s %q: %w",
				hash.name, hash.value, ErrProjectImageInvalid)
		}
		if !hash.required && hash.value != "" {
			return fmt.Errorf("project image %s environment carries %s: %w",
				e.ResolvedToolchain(), hash.name, ErrProjectImageInvalid)
		}
	}
	if !contentaddr.Valid(string(e.PreparationDigest)) {
		return fmt.Errorf("project image environment preparation_digest %q: %w",
			e.PreparationDigest, ErrProjectImageInvalid)
	}
	return nil
}

// InputLayout is where the image's environment inputs live in a tree: npm at
// the root for a record without an environment, which is admissible only at
// its build commit anyway.
func (p ProjectImage) InputLayout() ProjectImageInputLayout {
	if p.Environment == nil {
		return ProjectImageInputLayout{Toolchain: ProjectToolchainNPM}
	}
	return ProjectImageInputLayout{
		Toolchain: p.Environment.ResolvedToolchain(), GoModuleDir: p.Environment.GoModuleDir,
	}
}

// ChangedManifests names, in a fixed order, each dependency manifest of the
// environment's toolchain whose hash at base differs from the baked one: the
// root package.json and package-lock.json for npm, the module's go.mod and
// go.sum (as repository-relative paths) for Go. Empty means the base holds
// exactly the baked manifests.
func (e ProjectImageEnvironment) ChangedManifests(base ProjectImageBaseInputs) []string {
	type manifest struct{ name, baked, observed string }
	var manifests []manifest
	switch e.ResolvedToolchain() {
	case ProjectToolchainNPM:
		manifests = []manifest{
			{"package.json", e.PackageJSONSHA256, base.PackageJSONSHA256},
			{"package-lock.json", e.PackageLockSHA256, base.PackageLockSHA256},
		}
	case ProjectToolchainGo:
		manifests = []manifest{
			{path.Join(e.GoModuleDir, "go.mod"), e.GoModSHA256, base.GoModSHA256},
			{path.Join(e.GoModuleDir, "go.sum"), e.GoSumSHA256, base.GoSumSHA256},
		}
	}
	var changed []string
	for _, m := range manifests {
		if m.baked != m.observed {
			changed = append(changed, m.name)
		}
	}
	return changed
}

func (t ProjectToolchain) displayName() string {
	switch t {
	case ProjectToolchainNPM:
		return "npm"
	case ProjectToolchainGo:
		return "Go"
	}
	return string(t)
}

// AdmissibleAt reports whether the image may serve a run whose base is the
// observed commit, as nil or an ErrProjectImageIncompatible naming the first
// input that rules it out. currentPreparation is the running binary's
// preparation digest.
//
// The image's own build commit is always admissible, and for a record without
// an environment it is the only admissible base. Any other base is admissible
// only when every recorded input is unchanged there: both dependency
// manifests of the record's toolchain (for Go, in its module directory), the
// absence of inputs that toolchain does not support, and the preparation
// implementation; and when the base declares no recipe that contradicts the
// baked one. base must be observed under the image's InputLayout; a base read
// under another layout lacks the record's manifest hashes and is refused.
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
		return refuse("the base contains unsupported %s input %s",
			p.Environment.ResolvedToolchain().displayName(), strings.Join(base.UnsupportedInputs, ", "))
	}
	if changed := p.Environment.ChangedManifests(base); len(changed) != 0 {
		return refuse("%s at the base differs from the baked manifest", changed[0])
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
