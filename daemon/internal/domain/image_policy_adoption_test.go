package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func validAdoptionParameters() domain.ImagePolicyAdoptionParameters {
	recipe := domain.Digest("sha256:" + strings.Repeat("c1", 32))
	return domain.ImagePolicyAdoptionParameters{
		ProjectID:           "project-alpha",
		RefusalRecordID:     domain.Digest("sha256:" + strings.Repeat("e1", 32)),
		RecipeDigest:        &recipe,
		RegistryAuthorities: []string{"npm.pkg.github.com", "registry.npmjs.org"},
	}
}

func TestImagePolicyAdoptionGoldenAndRoundTrip(t *testing.T) {
	proposal, err := domain.NewImagePolicyAdoptionProposal(validAdoptionParameters())
	if err != nil {
		t.Fatal(err)
	}
	body, err := proposal.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	golden.Assert(t, "effect_proposal_image_policy_adoption", pretty.Bytes())
	decoded, err := domain.DecodeEffectProposal(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != proposal.Digest || decoded.AdoptionProposal == nil ||
		decoded.AdoptionProposal.ProjectID != "project-alpha" ||
		len(decoded.AdoptionProposal.RegistryAuthorities) != 2 ||
		decoded.ResolvedPolicyRunID != "" || decoded.ResolvedPolicyDigest != "" {
		t.Fatalf("decoded = %#v", decoded)
	}

	// Another kind's parameters beside the adoption arm break the union.
	mixed := decoded
	mixed.FilingProposal = &domain.FollowUpFilingParameters{}
	if err := mixed.Validate(); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("mixed arms: Validate = %v, want ErrEffectProposalInconsistent", err)
	}
}

// The card binds no resolved policy, at construction, in its body, and at
// the policy gate.
func TestImagePolicyAdoptionBindsNoResolvedPolicy(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewEffectProposal(
		domain.EffectImagePolicyAdoption, validAdoptionParameters(), proposalPolicy(t),
	); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("NewEffectProposal = %v, want ErrEffectProposalInconsistent", err)
	}
	proposal, err := domain.NewImagePolicyAdoptionProposal(validAdoptionParameters())
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.GateEffectProposal(proposal, proposalPolicy(t)); !errors.Is(err, domain.ErrProposalPolicyMismatch) {
		t.Fatalf("GateEffectProposal = %v, want ErrProposalPolicyMismatch", err)
	}
	policy := proposalPolicy(t)
	for name, mutate := range map[string]func(*domain.EffectProposal){
		"run id": func(p *domain.EffectProposal) { p.ResolvedPolicyRunID = policy.RunID },
		"digest": func(p *domain.EffectProposal) { p.ResolvedPolicyDigest = policy.Digest },
	} {
		bound := proposal
		mutate(&bound)
		digest, err := bound.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		bound.Digest = digest
		if err := bound.Validate(); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
			t.Errorf("%s: Validate = %v, want ErrEffectProposalInconsistent", name, err)
		}
	}

	// Every other kind still requires both policy fields.
	task := validEffectProposal(t)
	task.ResolvedPolicyRunID = ""
	if err := task.Validate(); !errors.Is(err, domain.ErrEmptyID) {
		t.Fatalf("task without policy run: Validate = %v, want ErrEmptyID", err)
	}
}

func TestImagePolicyAdoptionParametersValidate(t *testing.T) {
	t.Parallel()
	if err := validAdoptionParameters().Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	recipeOnly := validAdoptionParameters()
	recipeOnly.RegistryAuthorities = []string{}
	if err := recipeOnly.Validate(); err != nil {
		t.Fatalf("recipe only: %v", err)
	}
	authoritiesOnly := validAdoptionParameters()
	authoritiesOnly.RecipeDigest = nil
	if err := authoritiesOnly.Validate(); err != nil {
		t.Fatalf("authorities only: %v", err)
	}
	badRecipe := domain.Digest("sha256:short")
	tooMany := make([]string, domain.MaxImagePolicyAdoptionAuthorities+1)
	for i := range tooMany {
		tooMany[i] = "r" + strings.Repeat("a", i+1) + ".example.com"
	}
	for name, tc := range map[string]struct {
		mutate func(*domain.ImagePolicyAdoptionParameters)
		want   error
	}{
		"project id": {
			func(p *domain.ImagePolicyAdoptionParameters) { p.ProjectID = "" },
			domain.ErrEmptyID,
		},
		"refusal id": {
			func(p *domain.ImagePolicyAdoptionParameters) { p.RefusalRecordID = "" },
			domain.ErrEffectProposalInconsistent,
		},
		"recipe digest": {
			func(p *domain.ImagePolicyAdoptionParameters) { p.RecipeDigest = &badRecipe },
			domain.ErrEffectProposalInconsistent,
		},
		"null authorities": {
			func(p *domain.ImagePolicyAdoptionParameters) { p.RegistryAuthorities = nil },
			domain.ErrEffectProposalInconsistent,
		},
		"proposes nothing": {func(p *domain.ImagePolicyAdoptionParameters) {
			p.RecipeDigest, p.RegistryAuthorities = nil, []string{}
		}, domain.ErrEffectProposalInconsistent},
		"unsorted": {func(p *domain.ImagePolicyAdoptionParameters) {
			p.RegistryAuthorities = []string{"registry.npmjs.org", "npm.pkg.github.com"}
		}, domain.ErrEffectProposalInconsistent},
		"duplicate": {func(p *domain.ImagePolicyAdoptionParameters) {
			p.RegistryAuthorities = []string{"registry.npmjs.org", "registry.npmjs.org"}
		}, domain.ErrEffectProposalInconsistent},
		"not a host": {func(p *domain.ImagePolicyAdoptionParameters) {
			p.RegistryAuthorities = []string{"https://registry.npmjs.org"}
		}, domain.ErrRegistrySetInvalid},
		"too many": {
			func(p *domain.ImagePolicyAdoptionParameters) { p.RegistryAuthorities = tooMany },
			domain.ErrProposalParameterTooLarge,
		},
	} {
		params := validAdoptionParameters()
		tc.mutate(&params)
		if err := params.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", name, err, tc.want)
		}
	}
}

// adoptionGateFixture returns a project, a policy-class refusal for its
// repository naming both policy clauses, and a card for both.
func adoptionGateFixture(t *testing.T) (domain.Project, domain.ImageUpkeepRefusal, domain.ImagePolicyAdoptionParameters) {
	t.Helper()
	project, err := domain.NewProject("project-alpha", "freeasinbird/gh-imgup", 1278475858)
	if err != nil {
		t.Fatal(err)
	}
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: project.RepositoryID, Repository: project.Repo,
		Harness: domain.HarnessClientClaudeCode, TipSHA: strings.Repeat("e", 40),
		MeasuredFromImageID: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		Class:               domain.ImageUpkeepRefusalPolicy,
		Findings: []domain.ImageUpkeepClauseFinding{
			{Clause: domain.RebuildRefusalRecipeChanged, Detail: "recipe changed"},
			{Clause: domain.RebuildRefusalUndeclaredAuthority, Detail: "npm.pkg.github.com"},
		},
		RecordedAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	params := validAdoptionParameters()
	params.RefusalRecordID = refusal.ID
	return project, refusal, params
}

// refusalWith rebuilds base with another class and findings.
func refusalWith(
	t *testing.T, base domain.ImageUpkeepRefusal, class domain.ImageUpkeepRefusalClass,
	findings ...domain.ImageUpkeepClauseFinding,
) domain.ImageUpkeepRefusal {
	t.Helper()
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: base.RepositoryID, Repository: base.Repository, Harness: base.Harness,
		TipSHA: base.TipSHA, MeasuredFromImageID: base.MeasuredFromImageID,
		Class: class, Findings: findings, RecordedAt: base.RecordedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return refusal
}

func TestGateImagePolicyAdoption(t *testing.T) {
	t.Parallel()
	project, refusal, params := adoptionGateFixture(t)
	gate := func(params domain.ImagePolicyAdoptionParameters, project domain.Project, refusal domain.ImageUpkeepRefusal) error {
		t.Helper()
		proposal, err := domain.NewImagePolicyAdoptionProposal(params)
		if err != nil {
			t.Fatal(err)
		}
		return domain.GateImagePolicyAdoption(proposal, project, refusal)
	}
	if err := gate(params, project, refusal); err != nil {
		t.Fatalf("backed card: %v", err)
	}
	// no_registry_set backs hosts as undeclared_authority does.
	noSet := refusalWith(t, refusal, domain.ImageUpkeepRefusalPolicy,
		domain.ImageUpkeepClauseFinding{Clause: domain.RebuildRefusalNoRegistrySet, Detail: "none declared"})
	hostsOnly := params
	hostsOnly.RecipeDigest, hostsOnly.RefusalRecordID = nil, noSet.ID
	if err := gate(hostsOnly, project, noSet); err != nil {
		t.Fatalf("hosts on no_registry_set: %v", err)
	}

	otherProject, err := domain.NewProject("project-beta", project.Repo, project.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	otherRepository, err := domain.NewProject(project.ID, "freeasinbird/other", project.RepositoryID+1)
	if err != nil {
		t.Fatal(err)
	}
	repair := refusalWith(t, refusal, domain.ImageUpkeepRefusalRepair, refusal.Findings...)
	recipeOnlyRefusal := refusalWith(t, refusal, domain.ImageUpkeepRefusalPolicy,
		domain.ImageUpkeepClauseFinding{Clause: domain.RebuildRefusalRecipeChanged, Detail: "recipe changed"})
	hostsOnlyRefusal := refusalWith(t, refusal, domain.ImageUpkeepRefusalPolicy,
		domain.ImageUpkeepClauseFinding{Clause: domain.RebuildRefusalUndeclaredAuthority, Detail: "host"})
	withRefusal := func(id domain.Digest) domain.ImagePolicyAdoptionParameters {
		p := params
		p.RefusalRecordID = id
		return p
	}
	for name, tc := range map[string]struct {
		params  domain.ImagePolicyAdoptionParameters
		project domain.Project
		refusal domain.ImageUpkeepRefusal
	}{
		"other project":            {params, otherProject, refusal},
		"refusal for another card": {params, project, recipeOnlyRefusal},
		"repair class":             {withRefusal(repair.ID), project, repair},
		"another repository":       {params, otherRepository, refusal},
		"digest without clause":    {withRefusal(hostsOnlyRefusal.ID), project, hostsOnlyRefusal},
		"hosts without clause":     {withRefusal(recipeOnlyRefusal.ID), project, recipeOnlyRefusal},
	} {
		if err := gate(tc.params, tc.project, tc.refusal); !errors.Is(err, domain.ErrImagePolicyAdoptionUnbacked) {
			t.Errorf("%s: gate = %v, want ErrImagePolicyAdoptionUnbacked", name, err)
		}
	}

	task := validEffectProposal(t)
	if err := domain.GateImagePolicyAdoption(task, project, refusal); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("task proposal: gate = %v, want ErrEffectProposalInconsistent", err)
	}
}

func TestImagePolicyAdoptionRecord(t *testing.T) {
	t.Parallel()
	project, refusal, params := adoptionGateFixture(t)
	proposal, err := domain.NewImagePolicyAdoptionProposal(params)
	if err != nil {
		t.Fatal(err)
	}
	card := domain.ProposalInstance{
		ID:              "proposal-0123456789abcdef0123456789abcdef",
		Admission:       domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpkeepRefusal, RefusalRecordID: refusal.ID},
		ProposalBatchID: "batch-adoption", Proposal: proposal, CreatedAt: refusal.RecordedAt,
	}
	at := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)
	adoption, err := domain.NewImagePolicyAdoption(card, project, at)
	if err != nil {
		t.Fatal(err)
	}
	if adoption.RepositoryID != project.RepositoryID || adoption.ProposalInstanceID != card.ID ||
		adoption.RecipeDigest == nil || *adoption.RecipeDigest != *params.RecipeDigest ||
		len(adoption.RegistryAuthorities) != 2 {
		t.Fatalf("adoption = %#v", adoption)
	}
	// The ID addresses every fact but RecordedAt.
	later, err := domain.NewImagePolicyAdoption(card, project, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if later.ID != adoption.ID {
		t.Fatalf("recorded_at changed the id: %s != %s", later.ID, adoption.ID)
	}

	otherProject, err := domain.NewProject("project-beta", project.Repo, project.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.NewImagePolicyAdoption(card, otherProject, at); !errors.Is(err, domain.ErrImagePolicyAdoptionUnbacked) {
		t.Fatalf("other project: %v, want ErrImagePolicyAdoptionUnbacked", err)
	}
	taskCard := card
	taskCard.Proposal = validEffectProposal(t)
	if _, err := domain.NewImagePolicyAdoption(taskCard, project, at); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("task card: %v, want ErrEffectProposalInconsistent", err)
	}

	for name, tc := range map[string]struct {
		mutate func(*domain.ImagePolicyAdoption)
		want   error
	}{
		"repository id": {func(a *domain.ImagePolicyAdoption) { a.RepositoryID = 0 }, domain.ErrNonPositive},
		"repository":    {func(a *domain.ImagePolicyAdoption) { a.Repository = "../x" }, domain.ErrImagePolicyAdoptionInvalid},
		"instance":      {func(a *domain.ImagePolicyAdoption) { a.ProposalInstanceID = "" }, domain.ErrEmptyID},
		"adopts nothing": {func(a *domain.ImagePolicyAdoption) {
			a.RecipeDigest, a.RegistryAuthorities = nil, []string{}
		}, domain.ErrImagePolicyAdoptionInvalid},
		"null hosts":  {func(a *domain.ImagePolicyAdoption) { a.RegistryAuthorities = nil }, domain.ErrImagePolicyAdoptionInvalid},
		"recorded_at": {func(a *domain.ImagePolicyAdoption) { a.RecordedAt = time.Time{} }, domain.ErrMissingTimestamp},
		"not utc": {func(a *domain.ImagePolicyAdoption) {
			a.RecordedAt = a.RecordedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
		"id": {func(a *domain.ImagePolicyAdoption) { a.RepositoryID++ }, domain.ErrImagePolicyAdoptionInconsistent},
	} {
		mutated := adoption
		mutated.RegistryAuthorities = append([]string{}, adoption.RegistryAuthorities...)
		tc.mutate(&mutated)
		if err := mutated.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", name, err, tc.want)
		}
	}
}
