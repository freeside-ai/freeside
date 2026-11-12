package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// addProofSubstitute seeds an adopted identity named name with one
// enrollment and generation, copying the fixture's own, after edit adjusts
// them. It returns the enrollment.
func (f *agentAdmissionFixture) addProofSubstitute(
	t *testing.T, name string, edit func(*domain.AuthIdentity, *domain.ClientEnrollment),
) domain.ClientEnrollment {
	t.Helper()
	ctx := context.Background()
	identity, enrollment := f.identity, f.enrollment
	identity.ID, identity.AccountBinding = domain.AuthIdentityID(name), "acct-"+name
	identity.Interim.AuthStoreVolume = name + "-interim"
	enrollment.ID, enrollment.AuthIdentityID = domain.ClientEnrollmentID(name+"/claude_code"), identity.ID
	enrollment.AccountBinding = identity.AccountBinding
	if edit != nil {
		edit(&identity, &enrollment)
	}
	manifest := agentTestDigest("6")
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordAuthIdentity(ctx, identity, agentTestAt.Add(-4*time.Minute)); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(ctx, enrollment, agentTestAt.Add(-3*time.Minute)); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity.ID, domain.InvocationID("inv-adopt-"+name),
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, AuthStoreVolume: name + "-generation", StoreManifestDigest: manifest,
			}, agentTestAt.Add(-2*time.Minute), agentTestAt.Add(10*time.Minute))
		if err != nil {
			return err
		}
		_, err = tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: enrollment.ID, AuthStoreVolume: name + "-generation",
			StoreManifestDigest: manifest, LeaseFence: lease.Fence,
			AccountBinding: identity.AccountBinding, RecordedAt: agentTestAt.Add(-time.Minute),
		}, agentTestAt.Add(-time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return enrollment
}

// addUngeneratedEnrollment seeds an adopted identity named name with one
// enrollment and no generation.
func (f *agentAdmissionFixture) addUngeneratedEnrollment(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	identity, enrollment := f.identity, f.enrollment
	identity.ID, identity.AccountBinding = domain.AuthIdentityID(name), "acct-"+name
	identity.Interim.AuthStoreVolume = name + "-interim"
	enrollment.ID, enrollment.AuthIdentityID = domain.ClientEnrollmentID(name+"/claude_code"), identity.ID
	enrollment.AccountBinding = identity.AccountBinding
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordAuthIdentity(ctx, identity, agentTestAt.Add(-4*time.Minute)); err != nil {
			return err
		}
		return tx.RecordClientEnrollment(ctx, enrollment, agentTestAt.Add(-3*time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
}

// markCurrentGeneration records a credential-integrity mark on the
// enrollment's current generation.
func (f *agentAdmissionFixture) markCurrentGeneration(t *testing.T, id domain.ClientEnrollmentID) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		generation, err := tx.CurrentEnrollmentGeneration(ctx, id)
		if err != nil {
			return err
		}
		_, err = tx.RecordGenerationIntegrityMark(ctx, domain.GenerationIntegrityMark{
			EnrollmentID: id, Ordinal: generation.Ordinal,
			Finding: domain.CredentialIntegrityCorruption, ObservedAt: agentTestAt,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAdapterProofSubstitutes: the adapter proof's substitutes for the
// implementer are every other enrollment that could run its lineup line,
// sorted by enrollment ID. The line's own enrollment, a disabled identity, a
// marked generation, no generation, a catalog that serves another model or
// effort, an uncataloged enrollment on another harness client, a route the
// tree lacks, and a route that does not admit the selection's egress are
// left out. Each substitute runs the
// implementer's line on its own enrollment and generation.
func TestAdapterProofSubstitutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	zulu := f.addProofSubstitute(t, "claude-zulu", nil)
	alpha := f.addProofSubstitute(t, "claude-alpha", nil)
	cataloged := f.addProofSubstitute(t, "claude-cataloged", nil)
	f.recordCatalog(t, cataloged.ID, "claude-implementer-test", "high")
	f.addProofSubstitute(t, "claude-disabled", func(identity *domain.AuthIdentity, _ *domain.ClientEnrollment) {
		identity.Enabled = false
	})
	marked := f.addProofSubstitute(t, "claude-marked", nil)
	f.markCurrentGeneration(t, marked.ID)
	otherModel := f.addProofSubstitute(t, "claude-other-model", nil)
	f.recordCatalog(t, otherModel.ID, "claude-other-test", "high")
	otherEffort := f.addProofSubstitute(t, "claude-other-effort", nil)
	f.recordCatalog(t, otherEffort.ID, "claude-implementer-test", "low")
	f.addProofSubstitute(t, "codex-uncataloged", func(_ *domain.AuthIdentity, enrollment *domain.ClientEnrollment) {
		enrollment.ID = "codex-uncataloged/codex_cli"
		enrollment.HarnessClient = domain.HarnessClientCodexCLI
	})
	f.addProofSubstitute(t, "claude-no-route", func(_ *domain.AuthIdentity, enrollment *domain.ClientEnrollment) {
		enrollment.Route = "route-the-tree-lacks"
	})
	f.addUngeneratedEnrollment(t, "claude-no-generation")

	substitutes, err := f.selection.AdapterProofSubstitutes(ctx, f.store, domain.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	var got []domain.ClientEnrollmentID
	for _, substitute := range substitutes {
		got = append(got, substitute.Enrollment.ID)
	}
	if want := []domain.ClientEnrollmentID{alpha.ID, cataloged.ID, zulu.ID}; !slices.Equal(got, want) {
		t.Fatalf("substitutes = %v, want %v", got, want)
	}
	first := substitutes[0]
	if first.Line.Names.Enrollment != string(alpha.ID) || first.Line.Names.Model != "claude-implementer-test" ||
		first.Line.PromptDigest != agentTestPrompts[domain.RoleImplementer].Digest ||
		first.Identity.ID != alpha.AuthIdentityID || first.Generation.EnrollmentID != alpha.ID ||
		first.Generation.AuthStoreVolume != "claude-alpha-generation" {
		t.Fatalf("substitute = %+v, want the implementer's line on its own enrollment and generation", first)
	}
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Select(adapter); err != nil {
		t.Fatalf("substitute does not select under the Claude ward adapter: %v", err)
	}

	narrowed := f.selection
	narrowed.EffectiveEgress = append(slices.Clone(f.selection.EffectiveEgress), "api.other.example:443")
	if substitutes, err := narrowed.AdapterProofSubstitutes(ctx, f.store, domain.RoleImplementer); err != nil || substitutes != nil {
		t.Fatalf("substitutes whose route lacks an egress authority = %+v, %v; want none", substitutes, err)
	}
}
