package importer

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// FindingKind classifies one publish-blocking policy finding. The zero
// value "" is invalid by design.
type FindingKind string

const (
	// FindingNonRegularChange records a candidate adding, modifying, or
	// removing a non-regular entry (symlink, submodule, special file,
	// unusual mode). §5.6 names the whole class publish-blocking, and a
	// git tree cannot faithfully hold it, so it also blocks the commit.
	FindingNonRegularChange FindingKind = "non_regular_change"
	// FindingInvalidPathEntry records a manifest entry whose name is not
	// representable as a canonical UTF-8 path (the export contract's
	// invalid_path kind, raw bytes preserved in path_hex).
	FindingInvalidPathEntry FindingKind = "invalid_path_entry"
	// FindingBlobOmitted records a changed regular entry whose content
	// blob the exporter's caps omitted, so its content cannot enter the
	// tree (the contract's contained failure direction for a workspace
	// built to exceed the caps).
	FindingBlobOmitted FindingKind = "blob_omitted"
	// FindingAutomationControlPath records a change touching an
	// automation-control path (§5.5): CI executes those with implicit
	// GITHUB_TOKEN and OIDC authority even when no secret is named.
	FindingAutomationControlPath FindingKind = "automation_control_path"
	// FindingReviewerInstructionPath records a change touching a
	// reviewer-instruction path (§5.8). Detection is mandatory, but the
	// finding lifts with the advisory disposition (revision 42): the agent
	// and the Freeside-invoked reviewer compose their instructions from the
	// exact trusted base, so the candidate's copy cannot govern its own
	// review, and the edit publishes as reviewed content that the human
	// merge gate sees surfaced in the PR body.
	FindingReviewerInstructionPath FindingKind = "reviewer_instruction_path"
	// FindingGitMetadataPath records a change touching git metadata that
	// steers downstream checkout, diff, or submodule behaviour
	// (.gitmodules, .gitattributes).
	FindingGitMetadataPath FindingKind = "git_metadata_path"
	// FindingVerificationRecipePath records a change touching a trusted
	// verification recipe or its control config (§5.8 verification_recipes):
	// the recipe files Freeside loads only from an approved base govern what
	// verification runs, so an ordinary candidate must never edit them. This
	// is the import-stage control-plane class; it is distinct from the verify
	// package's separate verify-stage verification_control_path risk-flag.
	FindingVerificationRecipePath FindingKind = "verification_recipe_path"
	// FindingPromptsPolicyPath records a change touching prompts or policy
	// configuration (§5.8 prompts_and_policy): trusted control-plane content
	// loaded only from an approved base.
	FindingPromptsPolicyPath FindingKind = "prompts_policy_path"
	// FindingEgressTrustPath records a change touching egress policy or a
	// trust profile (§5.8 egress_and_trust_profiles): trusted control-plane
	// content loaded only from an approved base.
	FindingEgressTrustPath FindingKind = "egress_trust_path"
	// FindingMaterialityRulesPath records a change touching materiality rules
	// (§5.8 materiality_rules): the trusted control-plane config that decides
	// which changes require human review, loaded only from an approved base.
	FindingMaterialityRulesPath FindingKind = "materiality_rules_path"
	// FindingAllowlistViolation records a change outside the work unit's
	// declared path allowlist.
	FindingAllowlistViolation FindingKind = "allowlist_violation"
	// FindingSizeViolation records a changed file or change set
	// exceeding the import size policy.
	FindingSizeViolation FindingKind = "size_violation"
	// FindingPathCollision records candidate-introduced paths that
	// collide under case folding or Unicode normalization on a case- or
	// normalization-insensitive checkout (APFS, the reference
	// deployment).
	FindingPathCollision FindingKind = "path_collision"
	// FindingSecret records a best-effort secret-scan match (§5.4): a
	// high-signal token pattern in supported textual content.
	FindingSecret FindingKind = "secret"
	// FindingSecretScanSkipped records an added or modified file whose
	// content was not secret-scanned because it exceeded the scan cap.
	// Surfaced rather than silently skipped so a findings-free import
	// honestly means "everything in scope was scanned", never "a large
	// file slipped through unscanned" (§5.4 honest scope).
	FindingSecretScanSkipped FindingKind = "secret_scan_skipped"
	// FindingCommitPlanSecret records a secret found only after syntactic JSON
	// unescaping. It withholds construction so neither an agent-authored plan
	// commit nor the single-commit fallback can launder the escaped credential.
	FindingCommitPlanSecret FindingKind = "commit_plan_secret" //nolint:gosec // Finding name, not a credential.
)

// AllFindingKinds lists every valid FindingKind: the single place a new
// kind is registered, driving the table-driven tests.
var AllFindingKinds = []FindingKind{
	FindingNonRegularChange,
	FindingInvalidPathEntry,
	FindingBlobOmitted,
	FindingAutomationControlPath,
	FindingReviewerInstructionPath,
	FindingGitMetadataPath,
	FindingVerificationRecipePath,
	FindingPromptsPolicyPath,
	FindingEgressTrustPath,
	FindingMaterialityRulesPath,
	FindingAllowlistViolation,
	FindingSizeViolation,
	FindingPathCollision,
	FindingSecret,
	FindingSecretScanSkipped,
	FindingCommitPlanSecret,
}

// valid is the validity predicate; as a predicate it uses default.
func (k FindingKind) valid() bool {
	switch k {
	case FindingNonRegularChange, FindingInvalidPathEntry,
		FindingBlobOmitted, FindingAutomationControlPath,
		FindingReviewerInstructionPath, FindingGitMetadataPath,
		FindingVerificationRecipePath, FindingPromptsPolicyPath,
		FindingEgressTrustPath, FindingMaterialityRulesPath,
		FindingAllowlistViolation, FindingSizeViolation,
		FindingPathCollision, FindingSecret, FindingSecretScanSkipped,
		FindingCommitPlanSecret:
		return true
	default:
		return false
	}
}

// blocksCommit reports whether a finding of this kind withholds commit
// construction because the tree cannot faithfully represent the
// candidate. Policy-only findings return false: the commit exists for
// the §5.5 control-plane route, and the publication gate consumes the
// findings. The switch omits default so a new FindingKind must decide
// its blocking class; the trailing return covers the invalid zero
// value.
func (k FindingKind) blocksCommit() bool {
	switch k {
	case FindingNonRegularChange, FindingInvalidPathEntry, FindingBlobOmitted,
		FindingCommitPlanSecret:
		return true
	case FindingAutomationControlPath, FindingReviewerInstructionPath,
		FindingGitMetadataPath, FindingVerificationRecipePath,
		FindingPromptsPolicyPath, FindingEgressTrustPath,
		FindingMaterialityRulesPath, FindingAllowlistViolation,
		FindingSizeViolation, FindingPathCollision, FindingSecret,
		FindingSecretScanSkipped:
		return false
	}
	return false
}

// FindingProfile selects how a pipeline dispatches on import findings. It
// follows the daemon enum convention: every named member is nonempty and the
// zero value "" is invalid. The *default* (publish-strict) is not a "" member
// but the absence of a profile — a nil *FindingProfile on Policy — so a record
// written before the field existed decodes to nil and re-encodes with the key
// omitted (Policy.FindingProfile is optional and omitempty), keeping those
// records and the production publication task payload byte-identical.
type FindingProfile string

const (
	// FindingProfilePublishStrict fails the invocation on any finding at all:
	// repo-channel content is published, so every finding is publish-blocking.
	// It is also the behavior of a nil (absent) profile, so it need not be set
	// explicitly; naming it lets a caller state the strict choice.
	FindingProfilePublishStrict FindingProfile = "publish_strict"
	// FindingProfileSpecification tolerates the workspace-debris finding classes
	// because the elaboration stage publishes a typed JSON specification, never
	// workspace content, so an agent's incidental build/test output must not
	// definitively fail the invocation. Integrity, secret, and control-plane
	// findings stay fatal (see fatalForSpecification).
	FindingProfileSpecification FindingProfile = "specification"
)

// AllFindingProfiles is the single registration point for finding profiles.
var AllFindingProfiles = []FindingProfile{
	FindingProfilePublishStrict,
	FindingProfileSpecification,
}

func (p FindingProfile) valid() bool {
	switch p {
	case FindingProfilePublishStrict, FindingProfileSpecification:
		return true
	default:
		return false
	}
}

// Fatal reports whether this finding must terminally fail the invocation under
// the given profile. It is the exported dispatch a pipeline uses to partition
// a result's findings into definitively-fatal and tolerated. A nil profile is
// the default publish-strict behavior.
func (f Finding) Fatal(profile *FindingProfile) bool {
	return f.Kind.fatalForProfile(profile)
}

// Advisory reports whether the finding lifts with the advisory disposition
// (plan §5.8, revision 42): detected and surfaced, never fatal to the export
// and never publication-blocking. The stance is the control-plane
// category's, read from the domain predicate, so this package never names
// an advisory kind itself.
func (f Finding) Advisory() bool {
	return f.Kind.advisory()
}

// AllAdvisory reports whether every finding is advisory, which is the
// "clean" bar a publishable import meets; an empty set trivially qualifies.
func AllAdvisory(findings []Finding) bool {
	for _, f := range findings {
		if !f.Advisory() {
			return false
		}
	}
	return true
}

func (k FindingKind) advisory() bool {
	cat, ok := categoryFor(k)
	return ok && cat.Advisory()
}

// fatalForProfile dispatches per-profile. A nil (absent) profile is
// publish-strict. The switch omits default so a new profile must decide its
// stance; the trailing return fails closed, treating an unknown profile as
// fully publish-strict.
func (k FindingKind) fatalForProfile(profile *FindingProfile) bool {
	if profile == nil {
		return !k.advisory()
	}
	switch *profile {
	case FindingProfilePublishStrict:
		// Every finding blocks publication of repo-channel content except
		// an advisory one, which the export carries into publication as
		// surfaced content.
		return !k.advisory()
	case FindingProfileSpecification:
		return k.fatalForSpecification()
	}
	return true
}

// fatalForSpecification reports whether a finding of this kind stays fatal
// under the elaboration (specification) profile. The switch omits default so a
// new FindingKind must decide its class; the trailing return covers the
// invalid zero value, failing closed.
//
// The fatality table (design call recorded in issue #768):
//   - Inherently fatal: the four kinds that already withhold the commit itself
//     (blocksCommit) — the tree cannot represent the candidate at all.
//   - Stay fatal for elaboration: a tolerated secret would be durably copied
//     into the daemon CAS by persistRepositoryBlobs, and none of the
//     control-plane path classes (§5.5/§5.8) or git metadata is plausible
//     investigation debris — an elaborator editing CI, policy, reviewer
//     instructions, or git metadata is off-contract even unpublished.
//   - Tolerated debris: gitignored build/test output is out-of-allowlist and
//     routinely over the size and scan caps. Tolerating secret_scan_skipped
//     means large debris goes unscanned; accepted because nothing publishes
//     and a secret match on scanned content stays fatal.
func (k FindingKind) fatalForSpecification() bool {
	switch k {
	case FindingNonRegularChange, FindingInvalidPathEntry, FindingBlobOmitted,
		FindingCommitPlanSecret:
		return true
	case FindingSecret, FindingAutomationControlPath,
		FindingReviewerInstructionPath, FindingVerificationRecipePath,
		FindingPromptsPolicyPath, FindingEgressTrustPath,
		FindingMaterialityRulesPath, FindingGitMetadataPath:
		return true
	case FindingAllowlistViolation, FindingSizeViolation,
		FindingPathCollision, FindingSecretScanSkipped:
		return false
	}
	return true
}

// Finding is one publish-blocking policy finding. Path names the
// affected canonical path; PathHex carries raw name bytes for
// invalid_path entries (the two are mutually exclusive, as in the
// manifest). Rule and Line locate secret findings by rule id and
// 1-based line. Detail is daemon-authored context; no field ever
// carries candidate content bytes.
type Finding struct {
	Path    string      `json:"path,omitempty"`
	PathHex string      `json:"path_hex,omitempty"`
	Kind    FindingKind `json:"kind"`
	Rule    string      `json:"rule,omitempty"`
	Line    int         `json:"line,omitempty"`
	Detail  string      `json:"detail,omitempty"`
}

// Candidate lifts an import finding into the domain's CandidateFinding, the
// shape the publication gate consumes (plan §5.6, §5.8): it assigns the trust
// class the gate dispatches on and, for a §5.8 control-plane finding, its
// category. The importer's flat kinds map in without the domain package
// enumerating them. Import findings are always blocking at emission — a
// waiver is a downstream human decision — and every import finding originates
// in the import stage. The switch omits default so the exhaustive linter
// forces a new kind to be classed; an unclassed (invalid zero) kind falls
// through with an empty Class, which fails CandidateFinding.Validate closed.
func (f Finding) Candidate() domain.CandidateFinding {
	// A secret finding locates its match by rule id and 1-based line; the
	// domain CandidateFinding has neither field (it carries no candidate
	// content and locates only by path), so fold them into Detail. Two secret
	// matches in one file differ only by rule/line, so without this they lift
	// to identical findings, which NewCandidateAuthorization rejects as
	// duplicates — sinking the whole authorization rather than recording every
	// blocking secret. Rule/Line are set only on secret findings.
	detail := f.Detail
	if f.Rule != "" || f.Line != 0 {
		loc := fmt.Sprintf("rule=%s line=%d", f.Rule, f.Line)
		if detail == "" {
			detail = loc
		} else {
			detail = detail + "; " + loc
		}
	}
	cf := domain.CandidateFinding{
		Kind:        string(f.Kind),
		Origin:      domain.FindingOriginImport,
		Path:        f.Path,
		PathHex:     f.PathHex,
		Detail:      detail,
		Disposition: domain.DispositionBlocking,
	}
	switch f.Kind {
	case FindingNonRegularChange, FindingInvalidPathEntry, FindingBlobOmitted:
		cf.Class = domain.FindingClassImportIntegrity
	case FindingAllowlistViolation, FindingSizeViolation,
		FindingPathCollision, FindingGitMetadataPath:
		cf.Class = domain.FindingClassRepoChangePolicy
	case FindingSecret, FindingSecretScanSkipped, FindingCommitPlanSecret:
		cf.Class = domain.FindingClassSecret
	case FindingAutomationControlPath, FindingReviewerInstructionPath,
		FindingVerificationRecipePath, FindingPromptsPolicyPath,
		FindingEgressTrustPath, FindingMaterialityRulesPath:
		// The six §5.8 categories are the control-plane class; categoryFor
		// resolves each kind's category from the same controlPlaneClasses
		// table applyPolicy emits from, so class and category share one source.
		cat, _ := categoryFor(f.Kind)
		cf.Class = domain.FindingClassControlPlane
		cf.Category = &cat
		// The stance is the category's, not this package's: the domain
		// predicate is the single place that names which control-plane
		// category surfaces instead of blocking, and Validate rejects an
		// advisory lift for any other.
		if f.Kind.advisory() {
			cf.Disposition = domain.DispositionAdvisory
		}
	}
	return cf
}
