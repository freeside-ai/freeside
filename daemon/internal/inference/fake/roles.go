package fake

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// DefaultAuthorPrompt is the publication author's prompt when Roles names
// none. The author sites refuse an empty prompt, so the fixture carries one.
const DefaultAuthorPrompt = "Write the pull request for the reviewer who will merge it.\n"

// AuditedOn is the date on the launch proof Roles supplies.
const AuditedOn = "2026-09-09"

// Roles is a static inference.RoleSource: every judgment role resolves to one
// admissible call selection that Driver answers. The selection is a real
// resolution (a sealed route and the baseline call adapter, joined by
// domain.ResolveAgentSelection under the bridge), so the client's admission
// passes on its merits and a test that edits one fact sees the admission
// that fact fails.
type Roles struct {
	// Provider and Model are the route's service operator and the
	// selection's model; the client's producer label is "Provider/Model".
	// The model's lineage group is domain.LineageGroupOf's for claude_code.
	Provider string
	Model    string
	// Effort is the effort every role's selection names, as the level the
	// claude_code native table translates; empty is harness_default. The
	// baseline call adapter can send it.
	Effort domain.EffortLevel
	// WriterLineage is what the source reports for the writing roles' lines.
	WriterLineage map[domain.RoleName]string
	Credential    inference.Secret
	Driver        inference.Driver
	// AuthorPrompt is the publication author's prompt file; empty selects
	// DefaultAuthorPrompt.
	AuthorPrompt []byte
	// Edit, when set, changes what the source returns for a role after the
	// admissible answer is built. An error is the source's own refusal.
	Edit func(domain.RoleName, *inference.RoleCall) error
}

// Prompt returns the prompt Roles resolves for a role, which is also what its
// lineup line names.
func (r Roles) Prompt(role domain.RoleName) (inference.RolePrompt, bool) {
	if role == domain.RolePublicationAuthor {
		body := r.AuthorPrompt
		if len(body) == 0 {
			body = []byte(DefaultAuthorPrompt)
		}
		return inference.OperatorRolePrompt(role, body), true
	}
	return inference.CodeOwnedRolePrompt(role)
}

// ResolveRole implements inference.RoleSource.
func (r Roles) ResolveRole(_ context.Context, role domain.RoleName) (inference.RoleCall, error) {
	prompt, ok := r.Prompt(role)
	if !ok {
		return inference.RoleCall{}, fmt.Errorf("role %s has no judgment site", role)
	}
	const routeName = "fake_route"
	enrollment := domain.ClientEnrollment{
		ID: "fake-a/claude_code", AuthIdentityID: "fake-a",
		HarnessClient: domain.HarnessClientClaudeCode, Route: routeName,
		AuthMethod: domain.AuthMethodSetupToken, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshExternal, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: "operator@example.com",
	}
	identity := domain.AuthIdentity{
		ID: "fake-a", Provider: domain.AuthProviderClaude, AuthStoreMutationLease: true,
		Enabled: true, AccountBinding: enrollment.AccountBinding, CostOwner: "operator",
		Interim: domain.InterimClientFacts{AuthStoreVolume: "fake-cred", RefreshStrategy: domain.RefreshOnDemand},
	}
	route := domain.RouteFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		ServiceOperator: r.Provider, Protocol: "fake_protocol",
		InferenceAuthorities: []string{"inference.invalid:443"},
		BillingMode:          "subscription", FallbackPolicy: "fail_closed",
		TermsBasisDate: "2026-10-01",
	}
	var err error
	if route.Digest, err = route.ComputeDigest(); err != nil {
		return inference.RoleCall{}, err
	}
	effort := r.Effort
	if effort == "" {
		effort = domain.EffortHarnessDefault
	}
	native, err := domain.TranslateEffort(domain.HarnessClientClaudeCode, effort)
	if err != nil {
		return inference.RoleCall{}, err
	}
	adapter, err := agentbaseline.ClaudeCallAdapter()
	if err != nil {
		return inference.RoleCall{}, err
	}
	names := domain.AgentSelectionNames{Enrollment: string(enrollment.ID), Model: r.Model, Effort: native.Effective()}
	selection, err := domain.ResolveAgentSelection(domain.AgentSelectionInput{
		Names: names, Identity: identity, Enrollment: enrollment, Adapter: adapter,
	})
	if err != nil {
		return inference.RoleCall{}, err
	}
	call := inference.RoleCall{
		Line: domain.LineupSelection{
			Names: names, PromptName: prompt.Name, PromptDigest: prompt.Digest,
		},
		LineupRevision: domain.Digest(contentaddr.Sum([]byte("fake lineup"))),
		Selection:      selection, Route: route, Adapter: adapter,
		Enrollment: enrollment,
		Generation: domain.EnrollmentGeneration{
			EnrollmentID: enrollment.ID, Ordinal: 1, AuthStoreVolume: "fake-cred",
			StoreManifestDigest: domain.Digest(contentaddr.Sum([]byte("fake store"))),
			LeaseFence:          1, AccountBinding: enrollment.AccountBinding,
			RecordedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		},
		LaunchProof: &domain.InterimCallLaunchAudit{
			AdapterDigest: adapter.Digest, HarnessBuild: adapter.HarnessBuild, AuditedOn: AuditedOn,
		},
		Prompt:        prompt,
		WriterLineage: r.WriterLineage,
		Driver:        r.Driver, Credential: r.Credential,
		CredentialSource: inference.CredentialSourceEnrollment,
	}
	if r.Edit != nil {
		if err := r.Edit(role, &call); err != nil {
			return inference.RoleCall{}, err
		}
	}
	return call, nil
}
