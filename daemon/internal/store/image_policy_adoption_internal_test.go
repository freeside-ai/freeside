package store

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	adoptingRepositoryID = 4242
	otherRepositoryID    = 4343
	adoptingProjectID    = domain.ProjectID("project-adopt")
	otherProjectID       = domain.ProjectID("project-other")
)

var (
	adoptionAt    = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	adoptedRecipe = domain.Digest("sha256:" + strings.Repeat("7a", 32))
	adoptedHosts  = []string{"proxy.golang.org", "registry.npmjs.org"}
)

// adoptionFixture is a store holding two projects in two repositories and a
// policy-class refusal for the adopting one that backs both a recipe and
// registry hosts.
type adoptionFixture struct {
	s       *Store
	refusal domain.ImageUpkeepRefusal
}

func newAdoptionFixture(t *testing.T) adoptionFixture {
	t.Helper()
	ctx := context.Background()
	s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	for _, project := range []struct {
		id   domain.ProjectID
		repo string
		rid  int64
	}{{adoptingProjectID, "owner/adopt", adoptingRepositoryID}, {otherProjectID, "owner/other", otherRepositoryID}} {
		p, err := domain.NewProject(project.id, project.repo, project.rid)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error { return tx.RegisterProject(ctx, p) }); err != nil {
			t.Fatal(err)
		}
	}
	fx := adoptionFixture{s: s}
	fx.refusal = fx.recordRefusal(t, adoptingRepositoryID, domain.ImageUpkeepRefusalPolicy,
		domain.RebuildRefusalRecipeChanged, domain.RebuildRefusalUndeclaredAuthority)
	return fx
}

func (fx adoptionFixture) recordRefusal(
	t *testing.T, repositoryID int64, class domain.ImageUpkeepRefusalClass, clauses ...domain.RebuildRefusal,
) domain.ImageUpkeepRefusal {
	t.Helper()
	ctx := context.Background()
	repo := "owner/adopt"
	if repositoryID == otherRepositoryID {
		repo = "owner/other"
	}
	findings := make([]domain.ImageUpkeepClauseFinding, 0, len(clauses))
	for _, clause := range clauses {
		findings = append(findings, domain.ImageUpkeepClauseFinding{Clause: clause, Detail: string(clause)})
	}
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: repositoryID, Repository: repo,
		Harness: domain.HarnessClientClaudeCode, TipSHA: strings.Repeat("e", 40),
		MeasuredFromImageID: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		Class:               class, Findings: findings, RecordedAt: adoptionAt.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.RecordImageUpkeepRefusal(ctx, refusal)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return refusal
}

func (fx adoptionFixture) params() domain.ImagePolicyAdoptionParameters {
	recipe := adoptedRecipe
	return domain.ImagePolicyAdoptionParameters{
		ProjectID: adoptingProjectID, RefusalRecordID: fx.refusal.ID,
		RecipeDigest: &recipe, RegistryAuthorities: slices.Clone(adoptedHosts),
	}
}

func (fx adoptionFixture) admit(
	t *testing.T, params domain.ImagePolicyAdoptionParameters,
) (domain.ProposalInstance, bool, error) {
	t.Helper()
	return fx.admitUnder(t, domain.ProposalAdmissionKey{
		Source: domain.ProposalSourceUpkeepRefusal, RefusalRecordID: params.RefusalRecordID,
	}, params)
}

func (fx adoptionFixture) admitUnder(
	t *testing.T, key domain.ProposalAdmissionKey, params domain.ImagePolicyAdoptionParameters,
) (domain.ProposalInstance, bool, error) {
	t.Helper()
	ctx := context.Background()
	proposal, err := domain.NewImagePolicyAdoptionProposal(params)
	if err != nil {
		t.Fatal(err)
	}
	var (
		instance domain.ProposalInstance
		created  bool
	)
	err = fx.s.Write(ctx, func(tx *WriteTx) error {
		var err error
		instance, created, err = tx.AllocateProposalInstance(ctx, key, "batch-adoption", proposal, adoptionAt)
		return err
	})
	return instance, created, err
}

// decide opens the card's item, stores a command carrying action, and records
// the decision.
func (fx adoptionFixture) decide(t *testing.T, instance domain.ProposalInstance, action domain.Action) {
	t.Helper()
	fx.decideIn(t, instance, action, adoptingProjectID)
}

// decideIn is decide with the card's item opened in project, so a test can
// bind the card to an item that is not its own.
func (fx adoptionFixture) decideIn(
	t *testing.T, instance domain.ProposalInstance, action domain.Action, project domain.ProjectID,
) {
	t.Helper()
	ctx := context.Background()
	if err := fx.s.Write(ctx, func(tx *WriteTx) error {
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ItemID(string(instance.ID) + "/effect"), ProjectID: project,
			Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: domain.SubjectID(instance.ProposalBatchID)},
			Type:    domain.AttentionEffectProposal, Priority: domain.PriorityNormal,
			Reason:            "Decide whether to adopt the image policy",
			RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDecline},
			EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		if err := tx.BindProposalItem(ctx, item.ID, instance.ID, instance.Proposal.Digest, nil); err != nil {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "command-" + string(action), DeviceID: "device-1",
			ItemID: item.ID, ItemVersion: item.ItemVersion,
			ArtifactDigests: item.ArtifactDigests, Action: action,
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		var selected *domain.Digest
		if action != domain.ActionDecline {
			selected = &instance.Proposal.Digest
		}
		return tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, action, selected, adoptionAt.Add(time.Hour))
	}); err != nil {
		t.Fatalf("decide %s: %v", action, err)
	}
}

func (fx adoptionFixture) record(
	t *testing.T, instanceID domain.ProposalInstanceID, at time.Time,
) (domain.ImagePolicyAdoption, error) {
	t.Helper()
	ctx := context.Background()
	var adoption domain.ImagePolicyAdoption
	err := fx.s.Write(ctx, func(tx *WriteTx) error {
		var err error
		adoption, err = tx.RecordImagePolicyAdoption(ctx, instanceID, at)
		return err
	})
	return adoption, err
}

// adopt admits, approves, and records the fixture's card.
func (fx adoptionFixture) adopt(t *testing.T) domain.ImagePolicyAdoption {
	t.Helper()
	instance, _, err := fx.admit(t, fx.params())
	if err != nil {
		t.Fatal(err)
	}
	fx.decide(t, instance, domain.ActionApprove)
	adoption, err := fx.record(t, instance.ID, adoptionAt.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return adoption
}

func TestImagePolicyAdoptionCardRecordsAdoption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newAdoptionFixture(t)

	instance, created, err := fx.admit(t, fx.params())
	if err != nil || !created {
		t.Fatalf("admit = created %v, %v", created, err)
	}
	if again, created, err := fx.admit(t, fx.params()); err != nil || created || again.ID != instance.ID {
		t.Fatalf("second admission = %q created %v, %v; want %q converged", again.ID, created, err, instance.ID)
	}
	if err := fx.s.Read(ctx, func(tx *ReadTx) error {
		stored, err := tx.GetProposalInstance(ctx, instance.ID)
		if err != nil {
			return err
		}
		if stored.Proposal.ResolvedPolicyRunID != "" || stored.Proposal.ResolvedPolicyDigest != "" ||
			stored.Proposal.Digest != instance.Proposal.Digest {
			t.Errorf("stored card = %+v", stored.Proposal)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.record(t, instance.ID, adoptionAt); !errors.Is(err, domain.ErrTransitionCommandMismatch) {
		t.Fatalf("record before a decision = %v, want ErrTransitionCommandMismatch", err)
	}

	fx.decide(t, instance, domain.ActionApprove)
	adoption, err := fx.record(t, instance.ID, adoptionAt.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if adoption.RepositoryID != adoptingRepositoryID || adoption.Repository != "owner/adopt" ||
		adoption.RefusalRecordID != fx.refusal.ID || adoption.RecipeDigest == nil ||
		*adoption.RecipeDigest != adoptedRecipe || !slices.Equal(adoption.RegistryAuthorities, adoptedHosts) {
		t.Fatalf("adoption = %+v", adoption)
	}
	// A repeated write converges on the first record and its recording time.
	again, err := fx.record(t, instance.ID, adoptionAt.Add(3*time.Hour))
	if err != nil || again.ID != adoption.ID || !again.RecordedAt.Equal(adoption.RecordedAt) {
		t.Fatalf("second record = %+v, %v; want the first", again, err)
	}

	if err := fx.s.Read(ctx, func(tx *ReadTx) error {
		for _, tc := range []struct {
			repositoryID int64
			adopted      bool
		}{{adoptingRepositoryID, true}, {otherRepositoryID, false}} {
			list, err := tx.ListImagePolicyAdoptions(ctx, tc.repositoryID)
			if err != nil {
				return err
			}
			hosts, err := tx.AdoptedRegistryHosts(ctx, tc.repositoryID)
			if err != nil {
				return err
			}
			recipes, err := tx.EffectiveApprovedRecipes(ctx, tc.repositoryID)
			if err != nil {
				return err
			}
			if (len(list) == 1) != tc.adopted || slices.Equal(hosts, adoptedHosts) != tc.adopted ||
				recipes[adoptedRecipe] != tc.adopted {
				t.Errorf("repository %d: adoptions %d, hosts %v, recipe adopted %v; want adopted %v",
					tc.repositoryID, len(list), hosts, recipes[adoptedRecipe], tc.adopted)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestImagePolicyAdoptionDeclinedCardRecordsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newAdoptionFixture(t)
	instance, _, err := fx.admit(t, fx.params())
	if err != nil {
		t.Fatal(err)
	}
	fx.decide(t, instance, domain.ActionDecline)
	if _, err := fx.record(t, instance.ID, adoptionAt); !errors.Is(err, domain.ErrTransitionCommandMismatch) {
		t.Fatalf("record after decline = %v, want ErrTransitionCommandMismatch", err)
	}
	if err := fx.s.Read(ctx, func(tx *ReadTx) error {
		list, err := tx.ListImagePolicyAdoptions(ctx, adoptingRepositoryID)
		if len(list) != 0 {
			t.Errorf("declined card left adoptions %+v", list)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestImagePolicyAdoptionAdmissionRefusesUnbackedCard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		params func(t *testing.T, fx adoptionFixture) domain.ImagePolicyAdoptionParameters
	}{
		{"repair-class refusal", func(t *testing.T, fx adoptionFixture) domain.ImagePolicyAdoptionParameters {
			p := fx.params()
			p.RefusalRecordID = fx.recordRefusal(t, adoptingRepositoryID, domain.ImageUpkeepRefusalRepair,
				domain.RebuildRefusalRecipeChanged, domain.RebuildRefusalUndeclaredAuthority).ID
			return p
		}},
		{"another repository's refusal", func(t *testing.T, fx adoptionFixture) domain.ImagePolicyAdoptionParameters {
			p := fx.params()
			p.RefusalRecordID = fx.recordRefusal(t, otherRepositoryID, domain.ImageUpkeepRefusalPolicy,
				domain.RebuildRefusalRecipeChanged, domain.RebuildRefusalUndeclaredAuthority).ID
			return p
		}},
		{"missing refusal", func(_ *testing.T, fx adoptionFixture) domain.ImagePolicyAdoptionParameters {
			p := fx.params()
			p.RefusalRecordID = domain.Digest("sha256:" + strings.Repeat("0f", 32))
			return p
		}},
		{"missing project", func(_ *testing.T, fx adoptionFixture) domain.ImagePolicyAdoptionParameters {
			p := fx.params()
			p.ProjectID = "project-missing"
			return p
		}},
		{"recipe without recipe_changed", func(t *testing.T, fx adoptionFixture) domain.ImagePolicyAdoptionParameters {
			p := fx.params()
			p.RefusalRecordID = fx.recordRefusal(t, adoptingRepositoryID, domain.ImageUpkeepRefusalPolicy,
				domain.RebuildRefusalUndeclaredAuthority).ID
			return p
		}},
		{"hosts without an authority finding", func(t *testing.T, fx adoptionFixture) domain.ImagePolicyAdoptionParameters {
			p := fx.params()
			p.RefusalRecordID = fx.recordRefusal(t, adoptingRepositoryID, domain.ImageUpkeepRefusalPolicy,
				domain.RebuildRefusalRecipeChanged).ID
			return p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newAdoptionFixture(t)
			if _, _, err := fx.admit(t, tc.params(t, fx)); !errors.Is(err, domain.ErrImagePolicyAdoptionUnbacked) {
				t.Fatalf("admit = %v, want ErrImagePolicyAdoptionUnbacked", err)
			}
		})
	}
}

// TestImagePolicyAdoptionAdmissionKeyNamesItsRefusal pins one card per
// refusal: an adoption card is admitted only under the upkeep_refusal key of
// the refusal it answers, and that source admits no other kind.
func TestImagePolicyAdoptionAdmissionKeyNamesItsRefusal(t *testing.T) {
	t.Parallel()
	fx := newAdoptionFixture(t)
	other := fx.recordRefusal(t, adoptingRepositoryID, domain.ImageUpkeepRefusalPolicy,
		domain.RebuildRefusalRecipeChanged)
	for name, key := range map[string]domain.ProposalAdmissionKey{
		"client command":  {Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "command-adopt"},
		"another refusal": {Source: domain.ProposalSourceUpkeepRefusal, RefusalRecordID: other.ID},
	} {
		if _, _, err := fx.admitUnder(t, key, fx.params()); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
			t.Errorf("admit under %s = %v, want ErrEffectProposalInconsistent", name, err)
		}
	}
	filing := domain.EffectProposal{Kind: domain.EffectFollowUpFiling}
	if err := gateAdmissionSource(domain.ProposalAdmissionKey{
		Source: domain.ProposalSourceUpkeepRefusal, RefusalRecordID: fx.refusal.ID,
	}, filing); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Errorf("filing under upkeep_refusal = %v, want ErrEffectProposalInconsistent", err)
	}
}

// TestImagePolicyAdoptionNeedsItsOwnItem pins that an approve recorded on an
// item outside the card's project adopts nothing, though the binding and the
// decision were stored.
func TestImagePolicyAdoptionNeedsItsOwnItem(t *testing.T) {
	t.Parallel()
	fx := newAdoptionFixture(t)
	instance, _, err := fx.admit(t, fx.params())
	if err != nil {
		t.Fatal(err)
	}
	fx.decideIn(t, instance, domain.ActionApprove, otherProjectID)
	if _, err := fx.record(t, instance.ID, adoptionAt); !errors.Is(err, domain.ErrTransitionCommandMismatch) {
		t.Fatalf("record through another project's item = %v, want ErrTransitionCommandMismatch", err)
	}
}

// TestImagePolicyAdoptionReadFailsClosed pins the read boundary: a stored
// adoption whose card was never approved, or whose columns disagree with its
// body, refuses every read of the effective set instead of widening it.
func TestImagePolicyAdoptionReadFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	effective := func(fx adoptionFixture) error {
		return fx.s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.EffectiveApprovedRecipes(ctx, adoptingRepositoryID)
			return err
		})
	}

	t.Run("declined card", func(t *testing.T) {
		t.Parallel()
		fx := newAdoptionFixture(t)
		instance, _, err := fx.admit(t, fx.params())
		if err != nil {
			t.Fatal(err)
		}
		fx.decide(t, instance, domain.ActionDecline)
		project, err := domain.NewProject(adoptingProjectID, "owner/adopt", adoptingRepositoryID)
		if err != nil {
			t.Fatal(err)
		}
		forged, err := domain.NewImagePolicyAdoption(instance, project, adoptionAt)
		if err != nil {
			t.Fatal(err)
		}
		body, err := encode(forged)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fx.s.db.ExecContext(ctx, recordImagePolicyAdoptionSQL, forged.ID, forged.RepositoryID,
			forged.ProposalInstanceID, forged.RefusalRecordID, formatTime(forged.RecordedAt), body); err != nil {
			t.Fatal(err)
		}
		if err := effective(fx); !errors.Is(err, domain.ErrTransitionCommandMismatch) {
			t.Fatalf("effective set over a forged adoption = %v, want ErrTransitionCommandMismatch", err)
		}
	})

	t.Run("unbacked card", func(t *testing.T) {
		t.Parallel()
		fx := newAdoptionFixture(t)
		params := fx.params()
		params.RefusalRecordID = fx.recordRefusal(t, adoptingRepositoryID, domain.ImageUpkeepRefusalRepair,
			domain.RebuildRefusalRecipeChanged, domain.RebuildRefusalUndeclaredAuthority).ID
		proposal, err := domain.NewImagePolicyAdoptionProposal(params)
		if err != nil {
			t.Fatal(err)
		}
		forged := domain.ProposalInstance{
			ID: "proposal-forged", ProposalBatchID: "batch-adoption", Proposal: proposal, CreatedAt: adoptionAt,
			Admission: domain.ProposalAdmissionKey{
				Source: domain.ProposalSourceUpkeepRefusal, RefusalRecordID: params.RefusalRecordID,
			},
		}
		key, err := forged.Admission.String()
		if err != nil {
			t.Fatal(err)
		}
		body, err := encode(forged)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fx.s.db.ExecContext(ctx, `INSERT INTO effect_proposal_instances
			(instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
			 resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
			VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?)`,
			forged.ID, key, forged.ProposalBatchID, proposal.Kind, proposal.Digest,
			domain.OpaqueSubjectHandle(params.ProjectID), formatTime(adoptionAt), body); err != nil {
			t.Fatal(err)
		}
		if err := fx.s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetProposalInstance(ctx, forged.ID)
			return err
		}); !errors.Is(err, domain.ErrImagePolicyAdoptionUnbacked) {
			t.Fatalf("read of a stored unbacked card = %v, want ErrImagePolicyAdoptionUnbacked", err)
		}
	})

	t.Run("column disagrees with body", func(t *testing.T) {
		t.Parallel()
		fx := newAdoptionFixture(t)
		adoption := fx.adopt(t)
		if _, err := fx.s.db.ExecContext(ctx, `UPDATE image_policy_adoptions SET repository_id = ? WHERE id = ?`,
			otherRepositoryID, adoption.ID); err != nil {
			t.Fatal(err)
		}
		if err := effective(fx); !errors.Is(err, errRowInconsistent) {
			t.Fatalf("effective set over a rewritten row = %v, want errRowInconsistent", err)
		}
	})
}

// adoptedEvidence is a verifier artifact whose recipe only an adoption
// approves.
func adoptedEvidence(t *testing.T) domain.Artifact {
	t.Helper()
	recipe := adoptedRecipe
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "art-adopted", Type: domain.ArtifactKindVerifyLog, Digest: "sha256:adopted-log",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "inv-adopted",
			HeadBinding: domain.HeadBound, SourceHeadSHA: "cafebabe",
			VerificationRecipeDigest: &recipe, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, map[domain.Digest]bool{adoptedRecipe: true})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func evidenceItem(t *testing.T, project domain.ProjectID, artifact domain.Artifact) domain.AttentionItem {
	t.Helper()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID("item-evidence-" + string(project)), ProjectID: project,
		Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: "batch-evidence"},
		Type:    domain.AttentionEffectProposal, Priority: domain.PriorityNormal,
		Reason:            "Evidence under an adopted recipe",
		RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDecline},
		EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, map[domain.Digest]bool{adoptedRecipe: true})
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// TestImagePolicyAdoptionWidensItsRepositoryEvidence pins where an adopted
// recipe counts: a standalone artifact under it persists, reads, and joins the
// backup closure; an item carrying it is approved in the adopting repository
// and refused in another; and a recipe-scoped read admits it only when the
// scope names it.
func TestImagePolicyAdoptionWidensItsRepositoryEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newAdoptionFixture(t)
	artifact := adoptedEvidence(t)
	put := func() error {
		return fx.s.Write(ctx, func(tx *WriteTx) error { return tx.PutArtifact(ctx, artifact) })
	}
	if err := put(); !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("PutArtifact before adoption = %v, want ErrPublishEligibleInconsistent", err)
	}

	fx.adopt(t)
	if err := put(); err != nil {
		t.Fatalf("PutArtifact after adoption: %v", err)
	}
	read := func(tx *ReadTx) error {
		_, err := tx.GetArtifact(ctx, artifact.ID)
		return err
	}
	if err := fx.s.Read(ctx, read); err != nil {
		t.Fatalf("GetArtifact after adoption: %v", err)
	}

	adopting := evidenceItem(t, adoptingProjectID, artifact)
	if err := fx.s.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, adopting) }); err != nil {
		t.Fatalf("item in the adopting repository: %v", err)
	}
	if err := fx.s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, adopting.ID)
		return err
	}); err != nil {
		t.Fatalf("read the adopting repository's item: %v", err)
	}
	other := evidenceItem(t, otherProjectID, artifact)
	if err := fx.s.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, other)
	}); !errors.Is(err, domain.ErrUnapprovedRecipe) {
		t.Fatalf("item in another repository = %v, want ErrUnapprovedRecipe", err)
	}

	closure, err := checkpointArtifactDigests(ctx, fx.s.db, fx.s.approvedRecipes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(closure.digests, artifact.Digest) {
		t.Fatalf("backup closure %v omits the adopted artifact", closure.digests)
	}

	if err := fx.s.ReadWithRecipeScope(ctx, nil, read); !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("empty scope = %v, want ErrPublishEligibleInconsistent", err)
	}
	if err := fx.s.ReadWithRecipeScope(ctx, []domain.Digest{adoptedRecipe}, read); err != nil {
		t.Fatalf("scope naming the adopted recipe: %v", err)
	}
}

// TestNoAdoptionLeavesRecipeSetUnchanged pins that a store with no adoption
// gates evidence against the operator's set alone.
func TestNoAdoptionLeavesRecipeSetUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newAdoptionFixture(t)
	if err := fx.s.Read(ctx, func(tx *ReadTx) error {
		recipes, err := tx.EffectiveApprovedRecipes(ctx, adoptingRepositoryID)
		if err != nil {
			return err
		}
		stored, err := tx.storeRecipeSet(ctx)
		if err != nil {
			return err
		}
		operator := map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true}
		if !maps.Equal(recipes, operator) || !maps.Equal(stored, operator) {
			t.Errorf("effective %v, store %v; want the operator's set %v", recipes, stored, operator)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
