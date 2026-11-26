package domain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ErrRecipeBindingInvalid marks a malformed recipe binding.
var ErrRecipeBindingInvalid = errors.New("recipe binding is invalid")

// RecipeBinding binds the verification recipe of a project that has no
// project image (plan §5.7, "A project may have no project image"): the
// recipe is read from the repository at the reviewed commit, approved as any
// recipe is, and bound by its own digest wherever a proof names the recipe.
// It is the project-settings section recipe_binding, and an admission pins it
// by value so a later settings change can't move an in-flight run's recipe.
//
// The binding vouches for no approval. Every use re-checks RecipeDigest
// against the currently approved recipes, as it does an image's recipe.
type RecipeBinding struct {
	Repository   string `json:"repository"`
	RepositoryID int64  `json:"repository_id"`
	// CommitSHA is the reviewed commit the recipe was read at.
	CommitSHA    string `json:"commit_sha"`
	RecipeDigest Digest `json:"recipe_digest"`
}

// Validate checks the binding's shape with the rules ProjectImage.Validate
// applies to the same four fields.
func (b RecipeBinding) Validate() error {
	if !projectRepositoryPattern.MatchString(b.Repository) || strings.Contains(b.Repository, "..") {
		return fmt.Errorf("recipe binding repository %q: %w", b.Repository, ErrRecipeBindingInvalid)
	}
	if b.RepositoryID <= 0 {
		return fmt.Errorf("recipe binding repository_id %d: %w", b.RepositoryID, ErrNonPositive)
	}
	if !projectCommitPattern.MatchString(b.CommitSHA) {
		return fmt.Errorf("recipe binding commit_sha %q: %w", b.CommitSHA, ErrRecipeBindingInvalid)
	}
	if !contentaddr.Valid(string(b.RecipeDigest)) {
		return fmt.Errorf("recipe binding recipe_digest %q: %w", b.RecipeDigest, ErrRecipeBindingInvalid)
	}
	return nil
}
