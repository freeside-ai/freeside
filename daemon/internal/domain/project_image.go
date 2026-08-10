package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

const projectImageEncodingVersion = "freeside.project-image/v1"

var (
	// ErrProjectImageInvalid marks a malformed project-image provenance field.
	ErrProjectImageInvalid = errors.New("project image provenance is invalid")
	// ErrProjectImageInconsistent marks a record whose ID does not address its
	// provenance and produced image.
	ErrProjectImageInconsistent = errors.New("project image identity does not match its content")
)

var (
	projectRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	projectCommitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ProjectImage is the immutable result of building one managed repository's
// runtime image. It binds the forge's stable repository identity, exact source
// commit, trusted recipe bytes by digest, workspace-preparation command, base
// image, and produced image. A run can therefore use ImageRef without losing
// which inputs made those bytes admissible.
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
func (p ProjectImage) ComputeID() (Digest, error) {
	body, err := json.Marshal(canonicalProjectImage{
		Version:            projectImageEncodingVersion,
		Repository:         p.Repository,
		RepositoryID:       p.RepositoryID,
		CommitSHA:          p.CommitSHA,
		RecipeDigest:       p.RecipeDigest,
		PreparationCommand: p.PreparationCommand,
		BaseImageRef:       p.BaseImageRef,
		ImageRef:           p.ImageRef,
	})
	if err != nil {
		return "", fmt.Errorf("project image id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
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
