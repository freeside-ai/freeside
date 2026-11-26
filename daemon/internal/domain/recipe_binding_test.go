package domain_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestRecipeBindingValidate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*domain.RecipeBinding)
		want   error
	}{
		"repository":    {func(b *domain.RecipeBinding) { b.Repository = "acme/../widgets" }, domain.ErrRecipeBindingInvalid},
		"repository id": {func(b *domain.RecipeBinding) { b.RepositoryID = 0 }, domain.ErrNonPositive},
		"commit sha":    {func(b *domain.RecipeBinding) { b.CommitSHA = "HEAD" }, domain.ErrRecipeBindingInvalid},
		"recipe digest": {func(b *domain.RecipeBinding) { b.RecipeDigest = "sha256:short" }, domain.ErrRecipeBindingInvalid},
	} {
		binding := recipeBindingFixture()
		tc.mutate(&binding)
		if err := binding.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", name, err, tc.want)
		}
	}
}
