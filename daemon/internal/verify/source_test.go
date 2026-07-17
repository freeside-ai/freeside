package verify

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

const testRecipePath = ".freeside/verify.json"

func TestLoadTrustedRecipeFromBase(t *testing.T) {
	dir, base := initRepo(t, map[string]string{testRecipePath: trustedRecipeBytes})
	g := newTestRunner(t, dir)
	got, err := loadTrustedRecipeBytes(context.Background(), g, BaseCommitRecipe(), base, testRecipePath, 1<<20)
	if err != nil {
		t.Fatalf("loadTrustedRecipeBytes: %v", err)
	}
	if !bytes.Equal(got, []byte(trustedRecipeBytes)) {
		t.Fatalf("loaded %q, want the base recipe bytes", got)
	}
}

func TestLoadTrustedRecipeFromConfig(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"README.md": "hello"})
	g := newTestRunner(t, dir)
	got, err := loadTrustedRecipeBytes(context.Background(), g, ConfigRecipe([]byte(trustedRecipeBytes)), base, testRecipePath, 1<<20)
	if err != nil {
		t.Fatalf("loadTrustedRecipeBytes: %v", err)
	}
	if !bytes.Equal(got, []byte(trustedRecipeBytes)) {
		t.Fatalf("loaded %q, want the config recipe bytes", got)
	}
}

func TestLoadTrustedRecipeFailsClosed(t *testing.T) {
	t.Run("absent at base", func(t *testing.T) {
		dir, base := initRepo(t, map[string]string{"README.md": "hello"})
		g := newTestRunner(t, dir)
		_, err := loadTrustedRecipeBytes(context.Background(), g, BaseCommitRecipe(), base, testRecipePath, 1<<20)
		if !errors.Is(err, ErrRecipeUnreadable) {
			t.Fatalf("err = %v, want ErrRecipeUnreadable", err)
		}
	})
	t.Run("directory at base", func(t *testing.T) {
		dir, base := initRepo(t, map[string]string{testRecipePath + "/nested": "x"})
		g := newTestRunner(t, dir)
		_, err := loadTrustedRecipeBytes(context.Background(), g, BaseCommitRecipe(), base, testRecipePath, 1<<20)
		if !errors.Is(err, ErrRecipeUnreadable) {
			t.Fatalf("err = %v, want ErrRecipeUnreadable", err)
		}
	})
	t.Run("beyond the read cap", func(t *testing.T) {
		dir, base := initRepo(t, map[string]string{testRecipePath: trustedRecipeBytes})
		g := newTestRunner(t, dir)
		_, err := loadTrustedRecipeBytes(context.Background(), g, BaseCommitRecipe(), base, testRecipePath, 8)
		if !errors.Is(err, ErrRecipeUnreadable) {
			t.Fatalf("err = %v, want ErrRecipeUnreadable", err)
		}
	})
	t.Run("unset source", func(t *testing.T) {
		dir, base := initRepo(t, map[string]string{testRecipePath: trustedRecipeBytes})
		g := newTestRunner(t, dir)
		_, err := loadTrustedRecipeBytes(context.Background(), g, RecipeSource{}, base, testRecipePath, 1<<20)
		if !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("err = %v, want ErrInvalidOptions", err)
		}
	})
}

// TestRecipeDivergence proves acceptance 1's detection half over real
// candidate commits: a workspace-modified recipe copy is flagged, and
// the trusted bytes are what the comparison holds against.
func TestRecipeDivergence(t *testing.T) {
	hostile := `{"commands": [["true"]], "capture": "none"}`
	cases := []struct {
		name       string
		baseFiles  map[string]string
		changes    map[string]string
		src        RecipeSource
		wantDetail string // "" means no finding
	}{
		{
			name:       "modified at head",
			baseFiles:  map[string]string{testRecipePath: trustedRecipeBytes},
			changes:    map[string]string{testRecipePath: hostile},
			src:        BaseCommitRecipe(),
			wantDetail: "differs",
		},
		{
			name:       "deleted at head",
			baseFiles:  map[string]string{testRecipePath: trustedRecipeBytes, "README.md": "keep"},
			changes:    map[string]string{testRecipePath: ""},
			src:        BaseCommitRecipe(),
			wantDetail: "deleted",
		},
		{
			name:       "added at head against config source",
			baseFiles:  map[string]string{"README.md": "keep"},
			changes:    map[string]string{testRecipePath: hostile},
			src:        ConfigRecipe([]byte(trustedRecipeBytes)),
			wantDetail: "differs",
		},
		{
			name:       "identical at head",
			baseFiles:  map[string]string{testRecipePath: trustedRecipeBytes},
			changes:    map[string]string{"main.go": "package main"},
			src:        BaseCommitRecipe(),
			wantDetail: "",
		},
		{
			name:       "absent at head against config source",
			baseFiles:  map[string]string{"README.md": "keep"},
			changes:    map[string]string{"main.go": "package main"},
			src:        ConfigRecipe([]byte(trustedRecipeBytes)),
			wantDetail: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, base := initRepo(t, tc.baseFiles)
			head := commitCandidate(t, dir, base, tc.changes)
			g := newTestRunner(t, dir)
			trusted, err := loadTrustedRecipeBytes(context.Background(), g, tc.src, base, testRecipePath, 1<<20)
			if err != nil {
				t.Fatalf("loadTrustedRecipeBytes: %v", err)
			}
			findings, err := recipeDivergence(context.Background(), g, tc.src, head, testRecipePath, trusted, 1<<20)
			if err != nil {
				t.Fatalf("recipeDivergence: %v", err)
			}
			if tc.wantDetail == "" {
				if len(findings) != 0 {
					t.Fatalf("findings = %v, want none", findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("findings = %v, want exactly one", findings)
			}
			f := findings[0]
			if f.Kind != FindingRecipeDivergence || f.Path != testRecipePath {
				t.Fatalf("finding = %+v, want recipe_divergence on %s", f, testRecipePath)
			}
			if !strings.Contains(f.Detail, tc.wantDetail) {
				t.Fatalf("detail %q does not mention %q", f.Detail, tc.wantDetail)
			}
		})
	}
}

// TestRecipeDivergenceNonBlobHead covers the candidate turning the
// recipe path into a directory: the trusted source still governs and
// the swap is flagged.
func TestRecipeDivergenceNonBlobHead(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"README.md": "keep"})
	head := commitCandidate(t, dir, base, map[string]string{testRecipePath + "/nested": "x"})
	g := newTestRunner(t, dir)
	findings, err := recipeDivergence(context.Background(), g, ConfigRecipe([]byte(trustedRecipeBytes)), head, testRecipePath, []byte(trustedRecipeBytes), 1<<20)
	if err != nil {
		t.Fatalf("recipeDivergence: %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != FindingRecipeDivergence {
		t.Fatalf("findings = %v, want one recipe_divergence", findings)
	}
}

func TestFindingKindValidity(t *testing.T) {
	for _, k := range AllFindingKinds {
		if !k.valid() {
			t.Errorf("registered finding kind %q reports invalid", k)
		}
	}
	if FindingKind("").valid() {
		t.Error("zero finding kind reports valid")
	}
}
