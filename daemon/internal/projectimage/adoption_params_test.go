package projectimage

import (
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// The card's facts are re-derived from the tree at the tip, measured from the
// image's build commit: the tip's recipe digest when it changed, and only the
// hosts outside the repository's effective registry set.
func TestDeriveUpkeepAdoptionNamesTheExactDigestAndHosts(t *testing.T) {
	const (
		declared    = `["registry.npmjs.org"]`
		otherRecipe = `{"commands":[["true"]],"capture":"none"}`
	)
	repo, base, image := newGateRepo(t)
	otherDigest := verify.RecipeDigest([]byte(otherRecipe))
	undeclared := gateHeadLock(gateResolvedFrom("https://registry.example.com/right/-/right-2.0.0.tgz"))
	for _, test := range []struct {
		name       string
		changes    map[string]string
		policy     string
		adopted    []string
		wantRecipe *domain.Digest
		wantHosts  []string
		adoptable  bool
	}{
		{
			name:    "recipe change with unchanged manifests",
			changes: map[string]string{verify.DefaultRecipePath: otherRecipe},
			policy:  declared, wantRecipe: &otherDigest, wantHosts: []string{}, adoptable: true,
		},
		{
			name: "undeclared host",
			changes: map[string]string{
				"package.json": gateHeadPackageJSON, "package-lock.json": undeclared,
			},
			policy: declared, wantHosts: []string{"registry.example.com"}, adoptable: true,
		},
		{
			// With no set declared, every host a changed entry resolves from
			// is outside it.
			name: "no registry set",
			changes: map[string]string{
				"package.json": gateHeadPackageJSON, "package-lock.json": gateHeadLock(gateDeclaredEntry),
			},
			wantHosts: []string{"registry.npmjs.org"}, adoptable: true,
		},
		{
			name: "host an earlier card adopted",
			changes: map[string]string{
				"package.json": gateHeadPackageJSON, "package-lock.json": undeclared,
				verify.DefaultRecipePath: otherRecipe,
			},
			policy: declared, adopted: []string{"registry.example.com"},
			wantRecipe: &otherDigest, wantHosts: []string{}, adoptable: true,
		},
		{
			// An unpinned source is a repair: no adoption answers it.
			name: "unpinned source beside a recipe change",
			changes: map[string]string{
				"package.json":           gateHeadPackageJSON,
				"package-lock.json":      gateHeadLock(gateResolvedFrom("http://registry.npmjs.org/right.tgz")),
				verify.DefaultRecipePath: otherRecipe,
			},
			policy: declared,
		},
		{
			name: "malformed declared set",
			changes: map[string]string{
				"package.json": gateHeadPackageJSON, "package-lock.json": undeclared,
			},
			policy: `["Registry.npmjs.org"]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo.git("checkout", "-q", "--detach", base)
			tip := repo.commit(test.changes)
			adoption, adoptable, err := DeriveUpkeepAdoption(
				t.Context(), "git", repo.dir, image, tip, gatePolicy(t, test.policy), test.adopted)
			if err != nil {
				t.Fatalf("DeriveUpkeepAdoption: %v", err)
			}
			if adoptable != test.adoptable {
				t.Fatalf("adoptable = %v, want %v", adoptable, test.adoptable)
			}
			if !adoptable {
				return
			}
			if (adoption.RecipeDigest == nil) != (test.wantRecipe == nil) ||
				(adoption.RecipeDigest != nil && *adoption.RecipeDigest != *test.wantRecipe) {
				t.Fatalf("recipe digest = %v, want %v", adoption.RecipeDigest, test.wantRecipe)
			}
			if adoption.Hosts == nil || !slices.Equal(adoption.Hosts, test.wantHosts) {
				t.Fatalf("hosts = %#v, want %#v", adoption.Hosts, test.wantHosts)
			}
		})
	}
}

// A Go module change whose registry set lacks the module proxy is a policy
// refusal the proxy host answers: the derivation names it, and once it is
// adopted the gate admits the change.
func TestDeriveUpkeepAdoptionNamesTheGoModuleProxy(t *testing.T) {
	const goMod = "module example.test/app\n\ngo 1.22\n"
	repo := newBaseInputsRepo(t)
	base := repo.commit(map[string]string{
		"go.mod": goMod, verify.DefaultRecipePath: testRecipe, "main.go": "package main\n",
	})
	image := domain.ProjectImage{
		CommitSHA: base, RecipeDigest: verify.RecipeDigest([]byte(testRecipe)),
		Environment: &domain.ProjectImageEnvironment{
			Toolchain: domain.ProjectToolchainGo, GoModSHA256: manifestSHA256([]byte(goMod)),
			PreparationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		},
	}
	tip := repo.commit(map[string]string{
		"go.mod": goMod + "\nrequire example.test/dep v1.0.0\n", "go.sum": testGoSum,
	})
	policy := gatePolicy(t, `["registry.npmjs.org"]`)

	decision, err := EvaluateUpkeep(t.Context(), "git", repo.dir, image, tip, policy, nil)
	if err != nil || !slices.Equal(findingClauses(decision.Findings),
		[]RebuildRefusal{RebuildRefusalUndeclaredAuthority}) {
		t.Fatalf("decision = %+v, %v; want the proxy refused", decision, err)
	}
	adoption, adoptable, err := DeriveUpkeepAdoption(t.Context(), "git", repo.dir, image, tip, policy, nil)
	if err != nil || !adoptable || adoption.RecipeDigest != nil ||
		!slices.Equal(adoption.Hosts, []string{goModuleProxyHost}) {
		t.Fatalf("adoption = %+v, %v, %v; want only %s", adoption, adoptable, err, goModuleProxyHost)
	}
	decision, err = EvaluateUpkeep(t.Context(), "git", repo.dir, image, tip, policy, adoption.Hosts)
	if err != nil || len(decision.Findings) != 0 {
		t.Fatalf("decision with the proxy adopted = %+v, %v; want it admitted", decision, err)
	}
}
