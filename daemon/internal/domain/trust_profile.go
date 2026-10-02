package domain

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// trustProfileEncodingVersion tags the canonical encoding ComputeDigest
// digests. Any change to the encoding (field set, ordering, separator
// discipline) is a new version: two daemon builds must never derive different
// digests for the same profile content, or the digest-bound publication gate
// (plan §5.5) would read an unchanged profile as drift across an upgrade.
// v6 replaces the falsified native-trigger review modes with the
// Freeside-invoked production binding. v5 binds the immutable GitHub repository ID so a renamed or deleted
// owner/name cannot authorize minting for a different repository. v4 adds
// explicit allow axes for every privilege WorkflowAudit attests:
// reusable workflows, package publishing, and artifact consumers. v3 added
// the commit_plan and message_ruleset policy keys (§5.5, §5.6
// commit-plan gating): a policy flip must change the profile digest, and a
// stored profile acquires the conservative single_commit default only
// through owner re-approval, never by silent injection into an
// already-approved digest. v2 added the prompts-and-policy,
// egress-and-trust, and materiality-rules widening lists to
// ProtectedPathConfig. Rows from a prior version fail Validate's digest
// recompute and are re-recorded by a human, never migrated (§5.5 drift
// recovery).
//
// The external_reviewers allowlist is the one field added without a bump.
// It is omitted from the canonical form when empty, so a profile without
// one encodes to the bytes it had before the field existed and keeps its
// approved digest; an empty allowlist admits nobody, which is what every
// such profile already meant. No two builds can disagree about a profile
// with entries, because no build before this field could hold one: an older
// build decoding such a body drops the unknown field, recomputes a
// different digest, and fails closed. A field whose absence would not mean
// "exactly as before" still needs a new version.
const trustProfileEncodingVersion = "freeside-trust-profile/v6"

// ProtectedPathConfig is the repository-specific widening of the protected
// control-plane path classes (plan §5.5, §5.8). Only Extra* fields exist by
// design: the mandatory-minimum default patterns live with the gates that
// enforce them and are not representable here, so no profile content can
// narrow or disable a default class; a profile can only widen a gate. The
// glob dialect is the importer's: path.Match segments with ** spanning
// directories. Every §5.8 ControlPlaneCategory has a widening list here
// (automation-control reaches workflow_configuration), so no category is
// enumerable but unreachable from config; git-metadata is an orthogonal
// integrity concern, not a §5.8 category.
type ProtectedPathConfig struct {
	ExtraAutomationControlPatterns   []string `json:"extra_automation_control_patterns"`
	ExtraReviewerInstructionPatterns []string `json:"extra_reviewer_instruction_patterns"`
	ExtraGitMetadataPatterns         []string `json:"extra_git_metadata_patterns"`
	ExtraVerificationControlPatterns []string `json:"extra_verification_control_patterns"`
	ExtraPromptsAndPolicyPatterns    []string `json:"extra_prompts_and_policy_patterns"`
	ExtraEgressAndTrustPatterns      []string `json:"extra_egress_and_trust_patterns"`
	ExtraMaterialityRulesPatterns    []string `json:"extra_materiality_rules_patterns"`
}

// Validate reports whether every pattern list is well-formed and canonical
// (sorted ascending, no duplicates). Canonical order makes the profile body a
// deterministic function of its content, so the content digest is
// order-independent and a reordered retry converges on the stored body.
func (c ProtectedPathConfig) Validate() error {
	for _, list := range []struct {
		name     string
		patterns []string
	}{
		{"extra_automation_control_patterns", c.ExtraAutomationControlPatterns},
		{"extra_reviewer_instruction_patterns", c.ExtraReviewerInstructionPatterns},
		{"extra_git_metadata_patterns", c.ExtraGitMetadataPatterns},
		{"extra_verification_control_patterns", c.ExtraVerificationControlPatterns},
		{"extra_prompts_and_policy_patterns", c.ExtraPromptsAndPolicyPatterns},
		{"extra_egress_and_trust_patterns", c.ExtraEgressAndTrustPatterns},
		{"extra_materiality_rules_patterns", c.ExtraMaterialityRulesPatterns},
	} {
		// A non-nil empty list is the same content as nil but a different
		// byte encoding ("[]" vs null); one representation per content is
		// what lets a write-once replay converge on the stored body.
		if list.patterns != nil && len(list.patterns) == 0 {
			return fmt.Errorf("protected paths %s: empty list must be nil: %w", list.name, ErrPatternsNotCanonical)
		}
		for i, pat := range list.patterns {
			if pat == "" {
				return fmt.Errorf("protected paths %s: %w", list.name, ErrEmptyField)
			}
			if pat[0] == '/' {
				return fmt.Errorf("protected paths %s %q: pattern must be repository-relative: %w", list.name, pat, ErrPatternsNotCanonical)
			}
			// Candidate paths are canonical: no empty, "." or ".." segments
			// ever appear in one, so a pattern containing them (a trailing
			// or doubled slash, a dot segment) is syntactically fine yet
			// matches nothing — a recorded, digested widening that silently
			// protects no path. Reject it rather than record it.
			for _, seg := range strings.Split(pat, "/") {
				if seg == "" || seg == "." || seg == ".." {
					return fmt.Errorf("protected paths %s %q: unmatchable %q segment: %w", list.name, pat, seg, ErrPatternsNotCanonical)
				}
			}
			// path.Match validates segment syntax; ** is a whole-segment
			// wildcard in the importer's dialect, so it passes unchanged.
			if _, err := path.Match(pat, ""); err != nil {
				return fmt.Errorf("protected paths %s %q: %w", list.name, pat, err)
			}
			if i > 0 && list.patterns[i-1] >= pat {
				return fmt.Errorf("protected paths %s %q after %q: %w", list.name, pat, list.patterns[i-1], ErrPatternsNotCanonical)
			}
		}
	}
	return nil
}

// canonicalize returns a copy with each pattern list sorted, deduplicated,
// and detached from the caller's backing arrays; empty lists collapse to nil
// so "no widening" has one representation.
func (c ProtectedPathConfig) canonicalize() ProtectedPathConfig {
	c.ExtraAutomationControlPatterns = canonicalPatterns(c.ExtraAutomationControlPatterns)
	c.ExtraReviewerInstructionPatterns = canonicalPatterns(c.ExtraReviewerInstructionPatterns)
	c.ExtraGitMetadataPatterns = canonicalPatterns(c.ExtraGitMetadataPatterns)
	c.ExtraVerificationControlPatterns = canonicalPatterns(c.ExtraVerificationControlPatterns)
	c.ExtraPromptsAndPolicyPatterns = canonicalPatterns(c.ExtraPromptsAndPolicyPatterns)
	c.ExtraEgressAndTrustPatterns = canonicalPatterns(c.ExtraEgressAndTrustPatterns)
	c.ExtraMaterialityRulesPatterns = canonicalPatterns(c.ExtraMaterialityRulesPatterns)
	return c
}

func canonicalPatterns(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	slices.Sort(out)
	return slices.Compact(out)
}

// ReviewSettings is the trust profile's Freeside-invoked review binding (plan
// §5.5, §7). Native GitHub review is observational evidence, not a trust
// setting and never an alternate way to satisfy this binding.
type ReviewSettings struct {
	Mode         ReviewMode `json:"mode"`
	ConfigDigest Digest     `json:"config_digest"`
}

// Validate reports whether the review settings are well-formed.
func (r ReviewSettings) Validate() error {
	if !r.Mode.valid() {
		return fmt.Errorf("review mode %q: %w", r.Mode, ErrInvalidReviewMode)
	}
	if r.ConfigDigest == "" {
		return fmt.Errorf("review config_digest: %w", ErrEmptyField)
	}
	return nil
}

// ExternalReviewer is one allowlist entry: a reviewer outside Freeside whose
// activity on a published pull request the owner admits (plan §5.19). The
// account ID is the identity, as repository_id is for the repository: a login
// can be renamed and later reused by someone else. The login is recorded as
// well, in the forge's own spelling, so the owner approves a name they can
// read and a rename admits nobody until they record a new profile.
type ExternalReviewer struct {
	Forge     ExternalReviewForge     `json:"forge"`
	AccountID int64                   `json:"account_id"`
	Login     string                  `json:"login"`
	Authority ExternalReviewAuthority `json:"authority"`
}

// Validate reports whether the entry is well-formed.
func (r ExternalReviewer) Validate() error {
	if !r.Forge.valid() {
		return fmt.Errorf("external reviewer forge %q: %w", r.Forge, ErrInvalidExternalReviewForge)
	}
	if r.AccountID <= 0 {
		return fmt.Errorf("external reviewer account_id %d: %w", r.AccountID, ErrNonPositive)
	}
	if err := validateExternalReviewerLogin(r.Login); err != nil {
		return err
	}
	if !r.Authority.valid() {
		return fmt.Errorf("external reviewer %q authority %q: %w", r.Login, r.Authority, ErrInvalidExternalReviewAuthority)
	}
	return nil
}

func validateExternalReviewerLogin(login string) error {
	if login == "" {
		return fmt.Errorf("external reviewer login: %w", ErrEmptyField)
	}
	if !utf8.ValidString(login) {
		return fmt.Errorf("external reviewer login: %w", ErrExternalReviewerLoginInvalid)
	}
	for _, r := range login {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("external reviewer login %q: %w", login, ErrExternalReviewerLoginInvalid)
		}
	}
	return nil
}

func compareExternalReviewerIdentity(a, b ExternalReviewer) int {
	return cmp.Or(cmp.Compare(a.Forge, b.Forge), cmp.Compare(a.AccountID, b.AccountID))
}

// canonicalExternalReviewers returns a detached copy sorted by forge then
// account ID with exact duplicates dropped; an empty list collapses to nil so
// "no allowlist" has one representation. Two entries for one account that
// differ in login or authority both survive, for Validate to reject: which
// one the owner meant is not this function's call.
func canonicalExternalReviewers(in []ExternalReviewer) []ExternalReviewer {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b ExternalReviewer) int {
		return cmp.Or(
			compareExternalReviewerIdentity(a, b),
			cmp.Compare(a.Login, b.Login),
			cmp.Compare(a.Authority, b.Authority),
		)
	})
	return slices.Compact(out)
}

// validateExternalReviewers reports whether the allowlist is well-formed and
// canonical: strictly ascending by forge then account ID, so one account has
// one entry and the stored body is a deterministic function of the content.
func validateExternalReviewers(list []ExternalReviewer) error {
	// A non-nil empty list is the nil content in a different in-memory
	// shape; one representation per content, as for the pattern lists.
	if list != nil && len(list) == 0 {
		return fmt.Errorf("external_reviewers: empty list must be nil: %w", ErrExternalReviewersNotCanonical)
	}
	for i, r := range list {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("external_reviewers[%d]: %w", i, err)
		}
		if i > 0 && compareExternalReviewerIdentity(list[i-1], r) >= 0 {
			return fmt.Errorf("external_reviewers[%d] %s:%d after %s:%d: %w",
				i, r.Forge, r.AccountID, list[i-1].Forge, list[i-1].AccountID, ErrExternalReviewersNotCanonical)
		}
	}
	return nil
}

// AutomationTrustProfile is the machine-readable per-repository trust profile
// (plan §5.5): the human-approved posture of the repository's automation
// authority. The daemon binds runs and publication to ProfileDigest; drift
// between the profile and the audited current state fails closed until a
// human records an approved new profile. ProfileDigest is exported so the
// type serializes, but it is computed from the content in
// NewAutomationTrustProfile and never taken from caller input; see
// AutomationTrustProfileInput.
type AutomationTrustProfile struct {
	Repo                       string                 `json:"repo"`
	RepositoryID               int64                  `json:"repository_id"`
	PRExecution                PRExecutionMode        `json:"pr_execution"`
	CandidateAutomationChanges AutomationChangePolicy `json:"candidate_automation_changes"`
	PRGitHubTokenPermissions   TokenPermissionsMode   `json:"pr_github_token_permissions"`
	AllowOIDC                  bool                   `json:"allow_oidc"`
	AllowEnvironmentSecrets    bool                   `json:"allow_environment_secrets"`
	AllowSecretBearingPRJobs   bool                   `json:"allow_secret_bearing_pr_jobs"`
	AllowSelfHostedCI          bool                   `json:"allow_self_hosted_ci"`
	AllowPullRequestTarget     bool                   `json:"allow_pull_request_target"`
	AllowReusableWorkflows     bool                   `json:"allow_reusable_workflows"`
	AllowPackagePublishing     bool                   `json:"allow_package_publishing"`
	AllowArtifactConsumers     bool                   `json:"allow_artifact_consumers"`
	CommitPlan                 CommitPlanMode         `json:"commit_plan"`
	MessageRuleset             MessageRuleset         `json:"message_ruleset"`
	WorkflowAuditDigest        Digest                 `json:"workflow_audit_digest"`
	Review                     ReviewSettings         `json:"review"`
	ProtectedPaths             ProtectedPathConfig    `json:"protected_paths"`
	// ExternalReviewers is omitted when empty so a profile without an
	// allowlist keeps the stored body and digest it had before the field
	// existed (see trustProfileEncodingVersion).
	ExternalReviewers []ExternalReviewer `json:"external_reviewers,omitempty"`
	ProfileDigest     Digest             `json:"profile_digest"`
}

// AutomationTrustProfileInput carries the caller-supplied fields of an
// AutomationTrustProfile. It has no ProfileDigest field: the digest is a
// content address computed by trusted construction, so there is no input
// path that can bind a profile to a digest its content does not resolve to.
type AutomationTrustProfileInput struct {
	Repo                       string
	RepositoryID               int64
	PRExecution                PRExecutionMode
	CandidateAutomationChanges AutomationChangePolicy
	PRGitHubTokenPermissions   TokenPermissionsMode
	AllowOIDC                  bool
	AllowEnvironmentSecrets    bool
	AllowSecretBearingPRJobs   bool
	AllowSelfHostedCI          bool
	AllowPullRequestTarget     bool
	AllowReusableWorkflows     bool
	AllowPackagePublishing     bool
	AllowArtifactConsumers     bool
	CommitPlan                 CommitPlanMode
	MessageRuleset             MessageRuleset
	WorkflowAuditDigest        Digest
	Review                     ReviewSettings
	ProtectedPaths             ProtectedPathConfig
	ExternalReviewers          []ExternalReviewer
}

// NewAutomationTrustProfile builds a validated profile whose protected-path
// lists and external-reviewer allowlist are canonical and whose ProfileDigest
// is computed from the content,
// so both are authentic by construction. Deserialization and literal paths
// that bypass this constructor are caught by Validate's recompute.
func NewAutomationTrustProfile(in AutomationTrustProfileInput) (AutomationTrustProfile, error) {
	p := AutomationTrustProfile{
		Repo:                       in.Repo,
		RepositoryID:               in.RepositoryID,
		PRExecution:                in.PRExecution,
		CandidateAutomationChanges: in.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   in.PRGitHubTokenPermissions,
		AllowOIDC:                  in.AllowOIDC,
		AllowEnvironmentSecrets:    in.AllowEnvironmentSecrets,
		AllowSecretBearingPRJobs:   in.AllowSecretBearingPRJobs,
		AllowSelfHostedCI:          in.AllowSelfHostedCI,
		AllowPullRequestTarget:     in.AllowPullRequestTarget,
		AllowReusableWorkflows:     in.AllowReusableWorkflows,
		AllowPackagePublishing:     in.AllowPackagePublishing,
		AllowArtifactConsumers:     in.AllowArtifactConsumers,
		CommitPlan:                 in.CommitPlan,
		MessageRuleset:             in.MessageRuleset,
		WorkflowAuditDigest:        in.WorkflowAuditDigest,
		Review:                     in.Review,
		ProtectedPaths:             in.ProtectedPaths.canonicalize(),
		ExternalReviewers:          canonicalExternalReviewers(in.ExternalReviewers),
	}
	digest, err := p.ComputeDigest()
	if err != nil {
		return AutomationTrustProfile{}, err
	}
	p.ProfileDigest = digest
	if err := p.Validate(); err != nil {
		return AutomationTrustProfile{}, err
	}
	return p, nil
}

// canonicalTrustProfile is the versioned canonical form whose JSON encoding
// is digested. Field order is pinned by the struct declaration and the
// profile golden test; changing either is an encoding-version bump.
type canonicalTrustProfile struct {
	Version                    string                 `json:"version"`
	Repo                       string                 `json:"repo"`
	RepositoryID               int64                  `json:"repository_id"`
	PRExecution                PRExecutionMode        `json:"pr_execution"`
	CandidateAutomationChanges AutomationChangePolicy `json:"candidate_automation_changes"`
	PRGitHubTokenPermissions   TokenPermissionsMode   `json:"pr_github_token_permissions"`
	AllowOIDC                  bool                   `json:"allow_oidc"`
	AllowEnvironmentSecrets    bool                   `json:"allow_environment_secrets"`
	AllowSecretBearingPRJobs   bool                   `json:"allow_secret_bearing_pr_jobs"`
	AllowSelfHostedCI          bool                   `json:"allow_self_hosted_ci"`
	AllowPullRequestTarget     bool                   `json:"allow_pull_request_target"`
	AllowReusableWorkflows     bool                   `json:"allow_reusable_workflows"`
	AllowPackagePublishing     bool                   `json:"allow_package_publishing"`
	AllowArtifactConsumers     bool                   `json:"allow_artifact_consumers"`
	CommitPlan                 CommitPlanMode         `json:"commit_plan"`
	MessageRuleset             MessageRuleset         `json:"message_ruleset"`
	WorkflowAuditDigest        Digest                 `json:"workflow_audit_digest"`
	Review                     ReviewSettings         `json:"review"`
	ProtectedPaths             ProtectedPathConfig    `json:"protected_paths"`
	ExternalReviewers          []ExternalReviewer     `json:"external_reviewers,omitempty"`
}

// ComputeDigest returns the content address of the profile: a sha256 over its
// versioned canonical serialization, every field except ProfileDigest itself.
// It canonicalizes the protected paths and the allowlist defensively so it is
// a true content address for any input; a value that also passes Validate is
// already canonical, so its stored body carries exactly the content these
// bytes address.
func (p AutomationTrustProfile) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(canonicalTrustProfile{
		Version:                    trustProfileEncodingVersion,
		Repo:                       p.Repo,
		RepositoryID:               p.RepositoryID,
		PRExecution:                p.PRExecution,
		CandidateAutomationChanges: p.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   p.PRGitHubTokenPermissions,
		AllowOIDC:                  p.AllowOIDC,
		AllowEnvironmentSecrets:    p.AllowEnvironmentSecrets,
		AllowSecretBearingPRJobs:   p.AllowSecretBearingPRJobs,
		AllowSelfHostedCI:          p.AllowSelfHostedCI,
		AllowPullRequestTarget:     p.AllowPullRequestTarget,
		AllowReusableWorkflows:     p.AllowReusableWorkflows,
		AllowPackagePublishing:     p.AllowPackagePublishing,
		AllowArtifactConsumers:     p.AllowArtifactConsumers,
		CommitPlan:                 p.CommitPlan,
		MessageRuleset:             p.MessageRuleset,
		WorkflowAuditDigest:        p.WorkflowAuditDigest,
		Review:                     p.Review,
		ProtectedPaths:             p.ProtectedPaths.canonicalize(),
		ExternalReviewers:          canonicalExternalReviewers(p.ExternalReviewers),
	})
	if err != nil {
		return "", fmt.Errorf("trust profile digest: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the profile is well-formed and its ProfileDigest
// authentic. The digest is a content address, not a caller label: Validate
// recomputes it and rejects a mismatch, so a decoded or exported profile
// whose content was altered under a bound digest fails closed at every trust
// boundary that re-runs Validate (the store's encode/decode both do).
func (p AutomationTrustProfile) Validate() error {
	if p.Repo == "" {
		return fmt.Errorf("trust profile repo: %w", ErrEmptyField)
	}
	if p.RepositoryID <= 0 {
		return fmt.Errorf("trust profile repository_id %d: %w", p.RepositoryID, ErrNonPositive)
	}
	if !p.PRExecution.valid() {
		return fmt.Errorf("trust profile pr_execution %q: %w", p.PRExecution, ErrInvalidPRExecutionMode)
	}
	if !p.CandidateAutomationChanges.valid() {
		return fmt.Errorf("trust profile candidate_automation_changes %q: %w", p.CandidateAutomationChanges, ErrInvalidAutomationChanges)
	}
	if !p.PRGitHubTokenPermissions.valid() {
		return fmt.Errorf("trust profile pr_github_token_permissions %q: %w", p.PRGitHubTokenPermissions, ErrInvalidTokenPermissions)
	}
	if !p.CommitPlan.valid() {
		return fmt.Errorf("trust profile commit_plan %q: %w", p.CommitPlan, ErrInvalidCommitPlanMode)
	}
	// The registry check is the §5.5 profile-review gate: an identifier the
	// built-in registry does not carry fails closed here, so an unscreenable
	// ruleset can never be approved into a digest.
	if !p.MessageRuleset.valid() {
		return fmt.Errorf("trust profile message_ruleset %q: %w", p.MessageRuleset, ErrUnknownMessageRuleset)
	}
	if p.WorkflowAuditDigest == "" {
		return fmt.Errorf("trust profile workflow_audit_digest: %w", ErrEmptyField)
	}
	if err := p.Review.Validate(); err != nil {
		return fmt.Errorf("trust profile %s: %w", p.Repo, err)
	}
	if err := p.ProtectedPaths.Validate(); err != nil {
		return fmt.Errorf("trust profile %s: %w", p.Repo, err)
	}
	if err := validateExternalReviewers(p.ExternalReviewers); err != nil {
		return fmt.Errorf("trust profile %s: %w", p.Repo, err)
	}
	if p.ProfileDigest == "" {
		return fmt.Errorf("trust profile profile_digest: %w", ErrEmptyField)
	}
	computed, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	if p.ProfileDigest != computed {
		return fmt.Errorf("trust profile %s digest %q, content resolves to %q: %w", p.Repo, p.ProfileDigest, computed, ErrProfileDigestMismatch)
	}
	return nil
}

// ExternalReviewAuthorityFor reports the authority the profile grants an
// external reviewer. It matches only when forge, account ID, and login all
// equal one entry, so a renamed login and a reused login both get nothing.
// The profile must already have passed Validate.
func (p AutomationTrustProfile) ExternalReviewAuthorityFor(
	forge ExternalReviewForge, accountID int64, login string,
) (ExternalReviewAuthority, bool) {
	for _, r := range p.ExternalReviewers {
		if r.Forge == forge && r.AccountID == accountID && r.Login == login {
			return r.Authority, true
		}
	}
	return "", false
}

// WorkflowAudit is one audited snapshot of a repository's effective
// automation authority (plan §5.5): what a PR-triggered job could actually
// do at the audited commit, recorded as flat attested facts. It is an
// observation ledger, not policy: the drift comparison against the bound
// trust profile happens at the publication decision point, which consumes
// these rows. WorkflowAuditDigest is the daemon-computed content address of
// the audited automation-control surface; two identical audits at different
// times are two real observations.
type WorkflowAudit struct {
	Repo                string    `json:"repo"`
	AuditedCommitSHA    string    `json:"audited_commit_sha"`
	AuditedAt           time.Time `json:"audited_at"`
	WorkflowAuditDigest Digest    `json:"workflow_audit_digest"`
	// Evidence is the sensitive canonical body addressed by
	// WorkflowAuditDigest. It is retained separately from the append-only
	// observation facts and omitted from ordinary audit serialization.
	Evidence            *WorkflowAuditEvidence `json:"-"`
	EffectiveTokenPerms TokenPermissionsMode   `json:"effective_token_permissions"`
	OIDCAvailable       bool                   `json:"oidc_available"`
	EnvironmentSecrets  bool                   `json:"environment_secrets"`
	// SecretBearingPRJobs and PullRequestTarget attest the two highest-risk
	// PR-job privileges the profile gates (allow_secret_bearing_pr_jobs,
	// allow_pull_request_target): every profile allow_* axis has an attested
	// counterpart here, or drift on that axis would be invisible to the
	// decision-point comparison.
	SecretBearingPRJobs bool `json:"secret_bearing_pr_jobs"`
	PullRequestTarget   bool `json:"pull_request_target"`
	ReusableWorkflows   bool `json:"reusable_workflows"`
	SelfHostedRunners   bool `json:"self_hosted_runners"`
	PackagePublishing   bool `json:"package_publishing"`
	ArtifactConsumers   bool `json:"artifact_consuming_workflows"`
	// ReviewDecisionRef names the human decision record that reviewed this
	// audit, when one exists; the audit itself is an observation and may
	// precede review.
	ReviewDecisionRef string `json:"review_decision_ref,omitempty"`
}

// Validate reports whether the audit snapshot is well-formed.
func (a WorkflowAudit) Validate() error {
	if a.Repo == "" {
		return fmt.Errorf("workflow audit repo: %w", ErrEmptyField)
	}
	if a.AuditedCommitSHA == "" {
		return fmt.Errorf("workflow audit %s audited_commit_sha: %w", a.Repo, ErrEmptyField)
	}
	if a.AuditedAt.IsZero() {
		return fmt.Errorf("workflow audit %s audited_at: %w", a.Repo, ErrMissingTimestamp)
	}
	if a.WorkflowAuditDigest == "" {
		return fmt.Errorf("workflow audit %s workflow_audit_digest: %w", a.Repo, ErrEmptyField)
	}
	if a.Evidence != nil {
		if err := a.Evidence.ValidateBinding(a.Repo, a.WorkflowAuditDigest); err != nil {
			return err
		}
	}
	if !a.EffectiveTokenPerms.valid() {
		return fmt.Errorf("workflow audit %s effective_token_permissions %q: %w", a.Repo, a.EffectiveTokenPerms, ErrInvalidTokenPermissions)
	}
	return nil
}

// TrustDriftError names the automation-authority axis on which an observed
// workflow audit exceeded the approved trust profile (plan §5.5). It is the
// concrete form of ErrTrustProfileDrift, so a caller can both match the
// class (errors.Is against the sentinel) and report the specific axis
// (errors.As). Approved and Observed carry the two values for the message
// and for tests that assert which axis a fixture drifts.
type TrustDriftError struct {
	Axis     string
	Approved string
	Observed string
}

func (e *TrustDriftError) Error() string {
	return fmt.Sprintf("automation authority drifted on %s: approved %q, observed %q: %v",
		e.Axis, e.Approved, e.Observed, ErrTrustProfileDrift)
}

// Is reports the error as an ErrTrustProfileDrift so the publication gate
// can match the whole drift class with a single sentinel.
func (e *TrustDriftError) Is(target error) bool { return target == ErrTrustProfileDrift }

// EvaluateTrustDrift fails closed if the observed workflow audit is not a
// faithful, no-more-permissive match for the approved trust profile (plan
// §5.5): the drift comparison the publication decision point runs against a
// bound profile. It is a pure predicate over two already-validated records
// (the caller re-runs Validate on both — the #52 re-gate — before trusting
// either); it performs no I/O and consults no external policy.
//
// It compares axes explicitly rather than leaning on the audited-surface
// digest, because WorkflowAudit is a trusted-but-not-self-certifying
// observation (its digest is a content address of the audited files, not
// recomputed from the attested facts, per the #172 contract), so digest
// equality cannot be relied on to catch a settings-derived privilege that
// drifts out of band of those files. The WorkflowAuditDigest equality check
// still guards the file surface and repository settings folded into it
// (workflows, branch protection, rulesets), which have no attested bool.
//
// Every attested privilege the audit carries has an explicit profile axis and
// drifts when observed without approval. This one-to-one shape is deliberate:
// adding an audit fact without a corresponding comparison would make reality
// drift invisible, while adding a profile allowance without an observation
// would make approval unauditable.
//
// The first drift found is returned; the axis order is stable so a fixture's
// expected axis is deterministic.
func EvaluateTrustDrift(profile AutomationTrustProfile, audit WorkflowAudit) error {
	if audit.Repo != profile.Repo {
		return &TrustDriftError{Axis: "repo", Approved: profile.Repo, Observed: audit.Repo}
	}
	if audit.WorkflowAuditDigest != profile.WorkflowAuditDigest {
		return &TrustDriftError{
			Axis:     "workflow_audit_digest",
			Approved: string(profile.WorkflowAuditDigest),
			Observed: string(audit.WorkflowAuditDigest),
		}
	}
	// read_write observed under a read_only profile is the one token-mode
	// drift; the reverse (a less-permissive observation) is not drift.
	if audit.EffectiveTokenPerms == TokenPermissionsReadWrite && profile.PRGitHubTokenPermissions == TokenPermissionsReadOnly {
		return &TrustDriftError{
			Axis:     "token_permissions",
			Approved: string(profile.PRGitHubTokenPermissions),
			Observed: string(audit.EffectiveTokenPerms),
		}
	}
	for _, ax := range []struct {
		name     string
		observed bool
		allowed  bool
	}{
		{"oidc", audit.OIDCAvailable, profile.AllowOIDC},
		{"environment_secrets", audit.EnvironmentSecrets, profile.AllowEnvironmentSecrets},
		{"secret_bearing_pr_jobs", audit.SecretBearingPRJobs, profile.AllowSecretBearingPRJobs},
		{"self_hosted_runners", audit.SelfHostedRunners, profile.AllowSelfHostedCI},
		{"pull_request_target", audit.PullRequestTarget, profile.AllowPullRequestTarget},
		{"reusable_workflows", audit.ReusableWorkflows, profile.AllowReusableWorkflows},
		{"package_publishing", audit.PackagePublishing, profile.AllowPackagePublishing},
		{"artifact_consumers", audit.ArtifactConsumers, profile.AllowArtifactConsumers},
	} {
		if ax.observed && !ax.allowed {
			return &TrustDriftError{Axis: ax.name, Approved: "false", Observed: "true"}
		}
	}
	return nil
}
