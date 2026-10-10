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
// admissible call agent that Driver answers. The agent is a real closure (a
// sealed route, offer, and the baseline call adapter joined by
// domain.ResolveAgentDefinition), so the client's admission passes on its
// merits and a test that edits one fact sees the admission that fact fails.
type Roles struct {
	// Provider and Model are the route's service operator and the offer's
	// route model id; the client's producer label is "Provider/Model".
	Provider string
	Model    string
	// LineageGroup is the lineage of the offer every judgment role runs on.
	LineageGroup string
	// WriterLineage is what the source reports for the writing roles' offers.
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

var offerNotAfter = time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)

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
		ID: "fake-a", Provider: "claude", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, Enabled: true, AccountBinding: enrollment.AccountBinding, CostOwner: "operator",
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
	offer := domain.OfferFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		RouteModelID:    r.Model, LineageGroup: r.LineageGroup,
		IdentityStability: domain.IdentityOpaque,
		AllowedEfforts:    []domain.EffortLevel{domain.EffortHarnessDefault},
		PricingRevision:   "2026-10", NotAfter: offerNotAfter,
	}
	if offer.Digest, err = offer.ComputeDigest(); err != nil {
		return inference.RoleCall{}, err
	}
	adapter, err := agentbaseline.ClaudeCallAdapter()
	if err != nil {
		return inference.RoleCall{}, err
	}
	const agentName = "fake-call"
	agent, err := domain.ResolveAgentDefinition(domain.AgentResolutionInput{
		Source: domain.AgentSource{
			Name: agentName, Enrollment: string(enrollment.ID), Route: routeName,
			Adapter: agentbaseline.ClaudeCallAdapterName, Offer: "fake-offer",
			Effort: domain.EffortHarnessDefault,
		},
		Identity: identity, Enrollment: enrollment,
		Route: route, Adapter: adapter, Offer: offer, OfferRoute: routeName,
	})
	if err != nil {
		return inference.RoleCall{}, err
	}
	call := inference.RoleCall{
		Line: domain.LineupSelection{
			AgentName: agentName, AgentDigest: agent.Digest,
			PromptName: prompt.Name, PromptDigest: prompt.Digest,
		},
		LineupRevision: domain.Digest(contentaddr.Sum([]byte("fake lineup"))),
		Agent:          agent, Route: route, Adapter: adapter, Offer: offer,
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
		CredentialSource: inference.CredentialSourceInterimFlag,
	}
	if r.Edit != nil {
		if err := r.Edit(role, &call); err != nil {
			return inference.RoleCall{}, err
		}
	}
	return call, nil
}
