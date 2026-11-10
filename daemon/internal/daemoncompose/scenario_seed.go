package daemoncompose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	fakeforge "github.com/freeside-ai/freeside/daemon/internal/exec/fake/forge"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// The scenario world's fixed names. Nothing here reaches GitHub: the
// repository is served by the in-process forge and the App identity is the
// forge's token source.
const (
	scenarioRepo         = "freeside-scenario/scenario"
	scenarioRepositoryID = int64(1)
	ScenarioProjectID    = domain.ProjectID("project-scenario")
	scenarioBaseRef      = "main"
	ScenarioIdentityID   = domain.AuthIdentityID("auth-scenario")
	// The writer enrollment on the scenario identity.
	ScenarioAccountBinding = "acct-scenario"
	ScenarioEnrollmentID   = domain.ClientEnrollmentID("auth-scenario/claude_code")
	// The second subscription a SecondSubscription scenario seeds.
	scenarioSecondIdentityID     = domain.AuthIdentityID("auth-scenario-second")
	scenarioSecondAccountBinding = "acct-scenario-second"
	scenarioSecondEnrollmentID   = domain.ClientEnrollmentID("auth-scenario-second/claude_code")
	scenarioForgeToken           = "scenario-token"
)

// ScenarioSubscription is one Claude writer subscription the scenario seeds:
// an enabled identity on its own pool and its claude_code enrollment.
type ScenarioSubscription struct {
	Identity   domain.AuthIdentity
	Enrollment domain.ClientEnrollment
}

func newScenarioSubscription(
	id domain.AuthIdentityID, enrollment domain.ClientEnrollmentID, account string,
) ScenarioSubscription {
	return ScenarioSubscription{
		Identity: domain.AuthIdentity{
			ID: id, Provider: domain.AuthProviderClaude, AccountBinding: account,
			AuthStoreMutationLease: true, Enabled: true, CostOwner: "operator",
			Interim: domain.InterimClientFacts{
				AuthStoreVolume: string(id) + "-credentials", RefreshStrategy: domain.RefreshOnDemand,
			},
		},
		Enrollment: domain.ClientEnrollment{
			ID: enrollment, AuthIdentityID: id,
			HarnessClient: domain.HarnessClientClaudeCode, Route: "anthropic-subscription",
			AuthMethod: domain.AuthMethodSetupToken, CredentialMode: domain.CredentialSubscriptionContained,
			RefreshStrategy: domain.RefreshOnDemand, AccountBinding: account,
		},
	}
}

// Subscriptions returns the scenario identity's subscription and, with
// SecondSubscription, the second one.
func (s Scenario) Subscriptions() []ScenarioSubscription {
	subscriptions := []ScenarioSubscription{
		newScenarioSubscription(ScenarioIdentityID, ScenarioEnrollmentID, ScenarioAccountBinding),
	}
	if s.SecondSubscription {
		subscriptions = append(subscriptions, newScenarioSubscription(
			scenarioSecondIdentityID, scenarioSecondEnrollmentID, scenarioSecondAccountBinding))
	}
	return subscriptions
}

// ScenarioRecipe is the verification recipe the scenario's project image
// bakes. It runs one host process that always passes.
var ScenarioRecipe = []byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`)

var scenarioCommitAuthor = engine.ProductionCommitAuthor{AppSlug: "freeside-scenario", BotUserID: 1}

// scenarioWorld is what seeding leaves for the composition: the paths it
// laid out and the records admission and publication bind to.
type scenarioWorld struct {
	// Dir is <db dir>/scenario: the repository, the fakes' state, the
	// publication work directory, and the submit inputs all live under it.
	Dir     string
	BaseDir string
	BaseSHA string
	Image   domain.ProjectImage
	Profile domain.AutomationTrustProfile
	Audit   domain.WorkflowAudit
	Backend fake.RunnerBackend
}

// seedScenario lays out the scenario's world under the database's directory
// and records what admission and publication require, mirroring the
// production publication test harness (daemon/internal/integration,
// newProductionPublicationHarness). It is idempotent: a restart on the same
// root finds the repository and the records in place and changes nothing.
//
// The backend's and the Claude ward adapter's conformance proofs are claims
// the fakes cannot back. Only the ephemeral refusal in run keeps them out of
// a supervised store.
//
// lineupRevision is the revision of the tree the scenario admits through,
// which a seeded routing policy names.
func seedScenario(
	ctx context.Context, st *store.Store, dbPath string, sc Scenario, lineupRevision domain.Digest, now time.Time,
) (scenarioWorld, error) {
	world := scenarioWorld{Dir: filepath.Join(filepath.Dir(dbPath), "scenario")}
	world.BaseDir = filepath.Join(world.Dir, "repository")
	baseSHA, err := seedScenarioRepository(world.BaseDir, sc.BaseFiles)
	if err != nil {
		return scenarioWorld{}, fmt.Errorf("seed scenario repository: %w", err)
	}
	world.BaseSHA = baseSHA
	ceiling, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		return scenarioWorld{}, errors.New("seed scenario: fresh-vm backend class has no capability ceiling")
	}
	world.Backend = fake.RunnerBackend{
		BackendName: string(domain.BackendFreshVMReadOnlyVolumeHandoff),
		Caps:        exec.NewCapabilitySet(ceiling...),
	}
	if world.Image, err = domain.NewProjectImage(domain.ProjectImageInput{
		Repository: scenarioRepo, RepositoryID: scenarioRepositoryID,
		CommitSHA: baseSHA, RecipeDigest: verify.RecipeDigest(ScenarioRecipe),
		PreparationCommand: []string{"/usr/bin/true"},
		BaseImageRef:       scenarioImageRef("base", "1"),
		ImageRef:           scenarioImageRef("project", "2"),
	}); err != nil {
		return scenarioWorld{}, fmt.Errorf("seed scenario project image: %w", err)
	}
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/scenario","repo":"` + scenarioRepo + `","workflows":[]}`))
	if err != nil {
		return scenarioWorld{}, fmt.Errorf("seed scenario workflow audit: %w", err)
	}
	if world.Profile, err = domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: scenarioRepo, RepositoryID: scenarioRepositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanPlanPreferred, MessageRuleset: domain.MessageRulesetGitHub1,
		WorkflowAuditDigest: evidence.Digest(),
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: fake.DefaultReviewConfigurationDigest,
		},
	}); err != nil {
		return scenarioWorld{}, fmt.Errorf("seed scenario trust profile: %w", err)
	}
	world.Audit = domain.WorkflowAudit{
		Repo: scenarioRepo, AuditedCommitSHA: baseSHA, AuditedAt: now,
		WorkflowAuditDigest: world.Profile.WorkflowAuditDigest, Evidence: &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	if err := seedScenarioRecords(ctx, st, sc, world, ceiling, lineupRevision, now); err != nil {
		return scenarioWorld{}, err
	}
	if err := writeScenarioSubmission(world.Dir, sc); err != nil {
		return scenarioWorld{}, fmt.Errorf("write scenario submission: %w", err)
	}
	return world, nil
}

// seedScenarioRecords records the auth identities, project image, backend
// and adapter conformance, and trust profile in one transaction, so a crash
// leaves all of them or none. A store that already holds the trust profile
// was seeded.
//
// Each writer subscription is what lineup admission reads: an enabled
// identity, its enrollment, and a current generation. The enrollment also
// gives the pool a collector to own a refusal mark; without one a quota
// refusal would raise a failure card instead of parking. A second
// subscription comes with a routing policy that orders both enrollments for
// the implementer, so a park offers the second. A refusal that names no
// reset, which already keeps the task from parking, comes with an allowed
// reading on every pool that names none either, so the client has a latest
// reading to show beside the card's offers.
func seedScenarioRecords(
	ctx context.Context, st *store.Store, sc Scenario, world scenarioWorld,
	ceiling domain.CapabilitySnapshot, lineupRevision domain.Digest, now time.Time,
) error {
	subscriptions := sc.Subscriptions()
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		return fmt.Errorf("seed scenario adapter conformance: %w", err)
	}
	adapterConformance, err := domain.NewAdapterConformance(domain.AdapterConformanceInput{
		AdapterDigest: adapter.Digest, Outcome: domain.ConformancePassed,
		ProvedCapabilities: domain.NewLaunchCapabilitySet(domain.LaunchCapReadTools, domain.LaunchCapMutationTools,
			domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput, domain.LaunchCapContextSeverance,
			domain.LaunchCapRouteStoreContract),
		ProvedAt: now,
	})
	if err != nil {
		return fmt.Errorf("seed scenario adapter conformance: %w", err)
	}
	conformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: world.Backend.ConfigurationDigest(), Capabilities: ceiling, ProvedAt: now,
	})
	if err != nil {
		return fmt.Errorf("seed scenario conformance: %w", err)
	}
	err = st.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.LatestTrustProfile(ctx, scenarioRepo); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		generations := make([]domain.EnrollmentGeneration, len(subscriptions))
		for i, subscription := range subscriptions {
			if generations[i], err = seedScenarioSubscription(ctx, tx, subscription, now); err != nil {
				return err
			}
		}
		if sc.SecondSubscription {
			enrollments := make([]domain.ClientEnrollmentID, len(subscriptions))
			for i, subscription := range subscriptions {
				enrollments[i] = subscription.Enrollment.ID
			}
			if _, err := tx.AppendRoutingPolicy(ctx, domain.RoutingPolicyInput{
				Roles:  []domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Enrollments: enrollments}},
				Source: domain.RoutingPolicySourceCLISet, SetBy: "scenario", LineupRevision: lineupRevision,
			}, now); err != nil {
				return err
			}
		}
		if refusal := sc.QuotaRefusal; refusal != nil && refusal.ResetAfter == 0 {
			for i, subscription := range subscriptions {
				if err := seedScenarioResetlessReading(ctx, tx, subscription, generations[i], refusal.Window, now); err != nil {
					return err
				}
			}
		}
		for _, reviewer := range fake.ReviewIdentities() {
			if err := tx.RecordUsagePool(ctx, domain.UsagePool{
				ID: reviewer.EffectiveUsagePool(), Provider: reviewer.Provider, MaxParallelExecutions: 4,
			}, now); err != nil {
				return err
			}
			if err := tx.RecordAuthIdentity(ctx, reviewer, now); err != nil {
				return err
			}
		}
		if err := tx.RecordProjectImage(ctx, world.Image); err != nil {
			return err
		}
		if _, err := tx.RecordBackendConformance(ctx, conformance); err != nil {
			return err
		}
		if _, err := tx.RecordAdapterConformance(ctx, adapterConformance); err != nil {
			return err
		}
		return tx.RecordTrustProfile(ctx, world.Profile, now)
	})
	if err != nil {
		return fmt.Errorf("seed scenario records: %w", err)
	}
	return nil
}

// seedScenarioSubscription records the subscription's pool, identity, and
// enrollment, and adopts the enrollment's first generation under a lease it
// releases again, as enrollment does. It returns the generation.
func seedScenarioSubscription(
	ctx context.Context, tx *store.WriteTx, subscription ScenarioSubscription, now time.Time,
) (domain.EnrollmentGeneration, error) {
	identity, enrollment := subscription.Identity, subscription.Enrollment
	if err := tx.RecordUsagePool(ctx, domain.UsagePool{
		ID: identity.EffectiveUsagePool(), Provider: identity.Provider, MaxParallelExecutions: 4,
	}, now); err != nil {
		return domain.EnrollmentGeneration{}, err
	}
	if err := tx.RecordAuthIdentity(ctx, identity, now); err != nil {
		return domain.EnrollmentGeneration{}, err
	}
	if err := tx.RecordClientEnrollment(ctx, enrollment, now); err != nil {
		return domain.EnrollmentGeneration{}, err
	}
	volume := string(identity.ID) + "-generation"
	manifest := scenarioDigest([]byte(string(identity.ID) + " auth store"))
	holder := domain.InvocationID("inv-scenario-adopt-" + string(identity.ID))
	lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity.ID, holder, &domain.LeaseGenerationBinding{
		EnrollmentID: enrollment.ID, AuthStoreVolume: volume, StoreManifestDigest: manifest,
	}, now, now.Add(time.Minute))
	if err != nil {
		return domain.EnrollmentGeneration{}, err
	}
	generation, err := tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
		EnrollmentID: enrollment.ID, AuthStoreVolume: volume, StoreManifestDigest: manifest,
		LeaseFence: lease.Fence, AccountBinding: identity.AccountBinding, RecordedAt: now,
	}, now)
	if err != nil {
		return domain.EnrollmentGeneration{}, err
	}
	return generation, tx.ReleaseAuthStoreMutationLease(ctx, identity.ID, holder, lease.Fence, now)
}

// seedScenarioResetlessReading records a successful collection on the
// subscription's pool that reads window allowed and names no reset, so the
// pool's record supplies none for a refusal on it.
func seedScenarioResetlessReading(
	ctx context.Context, tx *store.WriteTx, subscription ScenarioSubscription,
	generation domain.EnrollmentGeneration, window string, now time.Time,
) error {
	return tx.RecordPoolUsageObservation(ctx, domain.PoolUsageObservation{
		Pool: subscription.Identity.EffectiveUsagePool(), Collector: subscription.Enrollment.ID,
		LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: now},
		Windows: []domain.WindowReading{{
			Window: window, EnrollmentID: subscription.Enrollment.ID, Generation: generation.Ordinal,
			Status: domain.WindowStatusAllowed, ObservedAt: now,
		}},
	}, now)
}

// seedScenarioRepository makes the scenario's one-commit repository, or
// reads back the one an earlier start made. The forge's git helper pins the
// identity and dates, so the base SHA is the same on every root.
func seedScenarioRepository(dir string, files map[string]string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if sha, err := scenarioGit(dir, "rev-parse", "--verify", "refs/heads/"+scenarioBaseRef); err == nil {
			return sha, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// A directory without the base commit is a start that failed part way,
	// before or after git init: begin again.
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := writeTree(dir, files); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", scenarioBaseRef},
		{"add", "-A"},
		{"commit", "-q", "-m", "Scenario base"},
	} {
		if _, err := scenarioGit(dir, args...); err != nil {
			return "", err
		}
	}
	return scenarioGit(dir, "rev-parse", "HEAD")
}

func scenarioGit(dir string, args ...string) (string, error) {
	out, code, err := fakeforge.Git(dir, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return "", fmt.Errorf("git %s exited %d: %s", strings.Join(args, " "), code, out)
	}
	return out, nil
}

func writeTree(dir string, files map[string]string) error {
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// writeScenarioSubmission writes the four inputs `freesided submit` reads
// for the scenario's task, under dir: task.md, policy.json,
// publication.json, and work-unit.json.
func writeScenarioSubmission(dir string, sc Scenario) error {
	provenance := domain.KeyProvenance{
		Source: domain.ProvenanceOverride,
		Digest: scenarioDigest([]byte("scenario " + sc.Name + " policy")),
	}
	var policy []domain.PolicyKey
	for _, key := range [][2]string{
		{"paths", strings.Join(sc.allowedPaths(), ",")},
		{"gates.spec_approval", "true"},
		{"specification.max_iterations", "4"},
		{"budgets.stage_active_time", "1h"},
		{"waiting.spec_approval_attention_after", "1h"},
		{"research.allowlist", "example.com"},
		{"research.max_response_bytes", "1024"},
	} {
		policy = append(policy, domain.PolicyKey{Key: key[0], Value: key[1], Provenance: provenance})
	}
	publication := engine.ProductionPublication{
		Title:        *sc.Specification.Title,
		Body:         "## Why\n\nThe " + sc.Name + " scenario's scripted change.\n",
		CommitAuthor: scenarioCommitAuthor,
	}
	inputs := map[string]any{
		"policy.json":      policy,
		"publication.json": publication,
		"work-unit.json":   map[string]string{"completion_criterion": string(domain.CompletionBoundPRMerged)},
	}
	files := map[string]string{"task.md": sc.Task}
	for name, value := range inputs {
		body, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		files[name] = string(body) + "\n"
	}
	return writeTree(dir, files)
}

func scenarioImageRef(name, fill string) domain.ImageRef {
	return domain.ImageRef("ghcr.io/freeside-ai/scenario-" + name + "@sha256:" + strings.Repeat(fill, 64))
}

func scenarioDigest(body []byte) domain.Digest {
	return domain.Digest(contentaddr.Sum(body))
}
