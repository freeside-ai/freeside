package importer

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// PlanProblemCode is a stable machine-readable token for one reason a commit
// plan would not be honored by Import. The zero value "" is invalid by design.
// Message-screening failures reuse commitMessageFindingKind's exact tokens, and
// blocking import findings reuse FindingKind's exact tokens.
type PlanProblemCode string

const (
	// Plan lifecycle and intake refusal codes.
	PlanProblemPlanMissing    PlanProblemCode = "plan_missing"
	PlanProblemNoChanges      PlanProblemCode = "no_changes"
	PlanProblemHandoffRefused PlanProblemCode = "handoff_refused"

	// Commit-plan structural codes.
	PlanProblemInvalidUTF8               PlanProblemCode = "plan_invalid_utf8"
	PlanProblemInvalidJSON               PlanProblemCode = "plan_invalid_json"
	PlanProblemTrailingData              PlanProblemCode = "plan_trailing_data"
	PlanProblemVersionNotFirst           PlanProblemCode = "plan_version_not_first"
	PlanProblemVersionUnsupported        PlanProblemCode = "plan_version_unsupported"
	PlanProblemGroupsInvalid             PlanProblemCode = "plan_groups_invalid"
	PlanProblemGroupsEmpty               PlanProblemCode = "plan_groups_empty"
	PlanProblemGroupCapExceeded          PlanProblemCode = "plan_group_cap_exceeded"
	PlanProblemPathCapExceeded           PlanProblemCode = "plan_path_cap_exceeded"
	PlanProblemGroupInvalid              PlanProblemCode = "plan_group_invalid"
	PlanProblemGroupNameEmpty            PlanProblemCode = "plan_group_name_empty"
	PlanProblemGroupDiscriminator        PlanProblemCode = "plan_group_discriminator"
	PlanProblemGroupRemainderInvalid     PlanProblemCode = "plan_group_remainder_invalid"
	PlanProblemGroupPathsInvalid         PlanProblemCode = "plan_group_paths_invalid"
	PlanProblemPathInvalid               PlanProblemCode = "plan_path_invalid"
	PlanProblemPathReserved              PlanProblemCode = "plan_path_reserved"
	PlanProblemPathGitMetadata           PlanProblemCode = "plan_path_git_metadata"
	PlanProblemPathNotInChanges          PlanProblemCode = "plan_path_not_in_changes"
	PlanProblemPathDuplicated            PlanProblemCode = "plan_path_duplicated"
	PlanProblemPathUncovered             PlanProblemCode = "plan_path_uncovered"
	PlanProblemIntermediateTreeCollision PlanProblemCode = "plan_intermediate_tree_collision"
	PlanProblemIntermediateTreeFileDir   PlanProblemCode = "plan_intermediate_tree_file_directory"

	// Message-screening codes (same tokens as commitMessageFindingKind).
	PlanProblemMessageOverCap        PlanProblemCode = PlanProblemCode(messageFindingOverCap)
	PlanProblemMessageEmpty          PlanProblemCode = PlanProblemCode(messageFindingEmpty)
	PlanProblemMessageControl        PlanProblemCode = PlanProblemCode(messageFindingControl)
	PlanProblemMessageCloseDirective PlanProblemCode = PlanProblemCode(messageFindingCloseDirective)
	PlanProblemMessageSkipDirective  PlanProblemCode = PlanProblemCode(messageFindingSkipDirective)
	PlanProblemMessageSpoofedTrailer PlanProblemCode = PlanProblemCode(messageFindingSpoofedTrailer)

	// Blocking import finding codes (same tokens as FindingKind).
	PlanProblemNonRegularChange        PlanProblemCode = PlanProblemCode(FindingNonRegularChange)
	PlanProblemInvalidPathEntry        PlanProblemCode = PlanProblemCode(FindingInvalidPathEntry)
	PlanProblemBlobOmitted             PlanProblemCode = PlanProblemCode(FindingBlobOmitted)
	PlanProblemAutomationControlPath   PlanProblemCode = PlanProblemCode(FindingAutomationControlPath)
	PlanProblemReviewerInstructionPath PlanProblemCode = PlanProblemCode(FindingReviewerInstructionPath)
	PlanProblemGitMetadataPath         PlanProblemCode = PlanProblemCode(FindingGitMetadataPath)
	PlanProblemVerificationRecipePath  PlanProblemCode = PlanProblemCode(FindingVerificationRecipePath)
	PlanProblemPromptsPolicyPath       PlanProblemCode = PlanProblemCode(FindingPromptsPolicyPath)
	PlanProblemEgressTrustPath         PlanProblemCode = PlanProblemCode(FindingEgressTrustPath)
	PlanProblemMaterialityRulesPath    PlanProblemCode = PlanProblemCode(FindingMaterialityRulesPath)
	PlanProblemAllowlistViolation      PlanProblemCode = PlanProblemCode(FindingAllowlistViolation)
	PlanProblemSizeViolation           PlanProblemCode = PlanProblemCode(FindingSizeViolation)
	PlanProblemPathCollision           PlanProblemCode = PlanProblemCode(FindingPathCollision)
	PlanProblemSecret                  PlanProblemCode = PlanProblemCode(FindingSecret)
	PlanProblemSecretScanSkipped       PlanProblemCode = PlanProblemCode(FindingSecretScanSkipped)
	PlanProblemCommitPlanSecret        PlanProblemCode = PlanProblemCode(FindingCommitPlanSecret)
)

// AllPlanProblemCodes lists every valid PlanProblemCode; it is the single
// registration point.
var AllPlanProblemCodes = []PlanProblemCode{
	PlanProblemPlanMissing,
	PlanProblemNoChanges,
	PlanProblemHandoffRefused,
	PlanProblemInvalidUTF8,
	PlanProblemInvalidJSON,
	PlanProblemTrailingData,
	PlanProblemVersionNotFirst,
	PlanProblemVersionUnsupported,
	PlanProblemGroupsInvalid,
	PlanProblemGroupsEmpty,
	PlanProblemGroupCapExceeded,
	PlanProblemPathCapExceeded,
	PlanProblemGroupInvalid,
	PlanProblemGroupNameEmpty,
	PlanProblemGroupDiscriminator,
	PlanProblemGroupRemainderInvalid,
	PlanProblemGroupPathsInvalid,
	PlanProblemPathInvalid,
	PlanProblemPathReserved,
	PlanProblemPathGitMetadata,
	PlanProblemPathNotInChanges,
	PlanProblemPathDuplicated,
	PlanProblemPathUncovered,
	PlanProblemIntermediateTreeCollision,
	PlanProblemIntermediateTreeFileDir,
	PlanProblemMessageOverCap,
	PlanProblemMessageEmpty,
	PlanProblemMessageControl,
	PlanProblemMessageCloseDirective,
	PlanProblemMessageSkipDirective,
	PlanProblemMessageSpoofedTrailer,
	PlanProblemNonRegularChange,
	PlanProblemInvalidPathEntry,
	PlanProblemBlobOmitted,
	PlanProblemAutomationControlPath,
	PlanProblemReviewerInstructionPath,
	PlanProblemGitMetadataPath,
	PlanProblemVerificationRecipePath,
	PlanProblemPromptsPolicyPath,
	PlanProblemEgressTrustPath,
	PlanProblemMaterialityRulesPath,
	PlanProblemAllowlistViolation,
	PlanProblemSizeViolation,
	PlanProblemPathCollision,
	PlanProblemSecret,
	PlanProblemSecretScanSkipped,
	PlanProblemCommitPlanSecret,
}

// valid is the validity predicate; as a predicate it uses default.
func (c PlanProblemCode) valid() bool {
	switch c {
	case PlanProblemPlanMissing,
		PlanProblemNoChanges,
		PlanProblemHandoffRefused,
		PlanProblemInvalidUTF8,
		PlanProblemInvalidJSON,
		PlanProblemTrailingData,
		PlanProblemVersionNotFirst,
		PlanProblemVersionUnsupported,
		PlanProblemGroupsInvalid,
		PlanProblemGroupsEmpty,
		PlanProblemGroupCapExceeded,
		PlanProblemPathCapExceeded,
		PlanProblemGroupInvalid,
		PlanProblemGroupNameEmpty,
		PlanProblemGroupDiscriminator,
		PlanProblemGroupRemainderInvalid,
		PlanProblemGroupPathsInvalid,
		PlanProblemPathInvalid,
		PlanProblemPathReserved,
		PlanProblemPathGitMetadata,
		PlanProblemPathNotInChanges,
		PlanProblemPathDuplicated,
		PlanProblemPathUncovered,
		PlanProblemIntermediateTreeCollision,
		PlanProblemIntermediateTreeFileDir,
		PlanProblemMessageOverCap,
		PlanProblemMessageEmpty,
		PlanProblemMessageControl,
		PlanProblemMessageCloseDirective,
		PlanProblemMessageSkipDirective,
		PlanProblemMessageSpoofedTrailer,
		PlanProblemNonRegularChange,
		PlanProblemInvalidPathEntry,
		PlanProblemBlobOmitted,
		PlanProblemAutomationControlPath,
		PlanProblemReviewerInstructionPath,
		PlanProblemGitMetadataPath,
		PlanProblemVerificationRecipePath,
		PlanProblemPromptsPolicyPath,
		PlanProblemEgressTrustPath,
		PlanProblemMaterialityRulesPath,
		PlanProblemAllowlistViolation,
		PlanProblemSizeViolation,
		PlanProblemPathCollision,
		PlanProblemSecret,
		PlanProblemSecretScanSkipped,
		PlanProblemCommitPlanSecret:
		return true
	default:
		return false
	}
}

// description returns the fixed, content-free message for each PlanProblemCode.
// The switch omits default so the exhaustive linter forces every new code to
// provide one; the trailing return covers the invalid zero value.
func (c PlanProblemCode) description() string {
	switch c {
	case PlanProblemPlanMissing:
		return "commit plan file .freeside-commit-plan.json is absent"
	case PlanProblemNoChanges:
		return "commit plan is present, but workspace has no changes relative to base; commit plan will not be honored"
	case PlanProblemHandoffRefused:
		return "handoff refused by importer intake"
	case PlanProblemInvalidUTF8:
		return "commit plan is not valid UTF-8"
	case PlanProblemInvalidJSON:
		return "commit plan is not valid JSON or violates the schema"
	case PlanProblemTrailingData:
		return "commit plan carries trailing data after the top-level JSON object"
	case PlanProblemVersionNotFirst:
		return "commit plan must put version as its first top-level field"
	case PlanProblemVersionUnsupported:
		return "commit plan version must be freeside.commit-plan/v1"
	case PlanProblemGroupsInvalid:
		return "commit plan groups must be a JSON array"
	case PlanProblemGroupsEmpty:
		return "commit plan must define at least one group"
	case PlanProblemGroupCapExceeded:
		return "commit plan exceeds the maximum number of groups"
	case PlanProblemPathCapExceeded:
		return "commit plan exceeds the maximum number of listed paths"
	case PlanProblemGroupInvalid:
		return "commit plan group must be a JSON object"
	case PlanProblemGroupNameEmpty:
		return "commit plan group name must not be empty or whitespace-only"
	case PlanProblemGroupDiscriminator:
		return "commit plan group must specify exactly one of paths or remainder"
	case PlanProblemGroupRemainderInvalid:
		return "commit plan remainder must be true and appear at most once on the last group"
	case PlanProblemGroupPathsInvalid:
		return "commit plan group paths must be a non-empty array of strings"
	case PlanProblemPathInvalid:
		return "commit plan path is not canonical or exceeds path caps"
	case PlanProblemPathReserved:
		return "commit plan path occupies a reserved Freeside channel"
	case PlanProblemPathGitMetadata:
		return "commit plan path contains a git metadata component"
	case PlanProblemPathNotInChanges:
		return "commit plan path is not in the derived change set"
	case PlanProblemPathDuplicated:
		return "commit plan path is assigned to more than one group or listed twice"
	case PlanProblemPathUncovered:
		return "changed path is not covered by any commit plan group"
	case PlanProblemIntermediateTreeCollision:
		return "group produces an intermediate tree with case or normalization path collisions"
	case PlanProblemIntermediateTreeFileDir:
		return "group produces an intermediate tree where a path is both a file and a directory"
	case PlanProblemMessageOverCap:
		return "commit message exceeds the maximum byte cap"
	case PlanProblemMessageEmpty:
		return "commit message is empty or whitespace-only"
	case PlanProblemMessageControl:
		return "commit message contains a forbidden control or format character"
	case PlanProblemMessageCloseDirective:
		return "commit message contains a forbidden GitHub issue-closing phrase"
	case PlanProblemMessageSkipDirective:
		return "commit message contains a forbidden CI-skip directive"
	case PlanProblemMessageSpoofedTrailer:
		return "commit message contains a forbidden or spoofable trailer line"
	case PlanProblemNonRegularChange:
		return "change touches a non-regular file (symlink, submodule, special file, or unusual mode)"
	case PlanProblemInvalidPathEntry:
		return "workspace contains a path that is not canonical UTF-8"
	case PlanProblemBlobOmitted:
		return "changed file content was withheld by export size caps"
	case PlanProblemAutomationControlPath:
		return "change touches an automation-control path"
	case PlanProblemReviewerInstructionPath:
		return "change touches a reviewer-instruction path"
	case PlanProblemGitMetadataPath:
		return "change touches git metadata"
	case PlanProblemVerificationRecipePath:
		return "change touches a trusted verification recipe path"
	case PlanProblemPromptsPolicyPath:
		return "change touches a trusted prompts or policy path"
	case PlanProblemEgressTrustPath:
		return "change touches a trusted egress or trust-profile path"
	case PlanProblemMaterialityRulesPath:
		return "change touches a trusted materiality-rules path"
	case PlanProblemAllowlistViolation:
		return "change is outside the declared path allowlist"
	case PlanProblemSizeViolation:
		return "change exceeds the import size policy"
	case PlanProblemPathCollision:
		return "change introduces paths that collide under case or normalization folding"
	case PlanProblemSecret:
		return "changed file matched a secret pattern"
	case PlanProblemSecretScanSkipped:
		return "changed file exceeded the secret-scan cap"
	case PlanProblemCommitPlanSecret:
		return "commit plan matched a secret pattern"
	}
	return ""
}

// planProblemCodeForMessage maps a message-screening finding kind to its
// PlanProblemCode. The switch omits default so exhaustive forces every new
// kind to be mapped.
func planProblemCodeForMessage(k commitMessageFindingKind) PlanProblemCode {
	switch k {
	case messageFindingOverCap:
		return PlanProblemMessageOverCap
	case messageFindingEmpty:
		return PlanProblemMessageEmpty
	case messageFindingControl:
		return PlanProblemMessageControl
	case messageFindingCloseDirective:
		return PlanProblemMessageCloseDirective
	case messageFindingSkipDirective:
		return PlanProblemMessageSkipDirective
	case messageFindingSpoofedTrailer:
		return PlanProblemMessageSpoofedTrailer
	}
	return ""
}

// planProblemCodeForFinding maps an import FindingKind to its PlanProblemCode.
// The switch omits default so exhaustive forces every new kind to be mapped.
func planProblemCodeForFinding(k FindingKind) PlanProblemCode {
	switch k {
	case FindingNonRegularChange:
		return PlanProblemNonRegularChange
	case FindingInvalidPathEntry:
		return PlanProblemInvalidPathEntry
	case FindingBlobOmitted:
		return PlanProblemBlobOmitted
	case FindingAutomationControlPath:
		return PlanProblemAutomationControlPath
	case FindingReviewerInstructionPath:
		return PlanProblemReviewerInstructionPath
	case FindingGitMetadataPath:
		return PlanProblemGitMetadataPath
	case FindingVerificationRecipePath:
		return PlanProblemVerificationRecipePath
	case FindingPromptsPolicyPath:
		return PlanProblemPromptsPolicyPath
	case FindingEgressTrustPath:
		return PlanProblemEgressTrustPath
	case FindingMaterialityRulesPath:
		return PlanProblemMaterialityRulesPath
	case FindingAllowlistViolation:
		return PlanProblemAllowlistViolation
	case FindingSizeViolation:
		return PlanProblemSizeViolation
	case FindingPathCollision:
		return PlanProblemPathCollision
	case FindingSecret:
		return PlanProblemSecret
	case FindingSecretScanSkipped:
		return PlanProblemSecretScanSkipped
	case FindingCommitPlanSecret:
		return PlanProblemCommitPlanSecret
	}
	return ""
}

// PlanProblem is one machine-readable reason a commit plan would not be
// honored by Import. It carries only a stable code, an optional 1-based group
// number and path, and a fixed description (or, for PlanProblemHandoffRefused,
// the importer's intake error message). It never repeats file bytes or matched
// secret text.
type PlanProblem struct {
	Code    PlanProblemCode `json:"code"`
	Group   int             `json:"group,omitempty"`
	Path    string          `json:"path,omitempty"`
	Message string          `json:"message"`
}

// PlanCheckResult is the outcome of CheckCommitPlan: whether Import under
// plan_preferred would honor the handoff's commit plan cleanly, and the
// problems that would prevent it when Honored is false.
type PlanCheckResult struct {
	Honored  bool          `json:"honored"`
	Problems []PlanProblem `json:"problems"`
}

// CheckCommitPlan runs the importer's intake and commit-plan checks on
// handoffDir against opts.BaseSHA in repoDir without writing to repoDir or
// constructing any git object or commit. It is a convenience only: it decides
// nothing for Import, and Import remains the authority at handoff.
//
// When opts.Policy.CommitPlan is empty, CheckCommitPlan evaluates under
// domain.CommitPlanPlanPreferred. Caller and environment failures (invalid
// options, missing base commit, unsupported repository, or git plumbing errors)
// return a non-nil error. Every handoff and plan outcome returns a nil error
// and a PlanCheckResult with Honored set accordingly.
func CheckCommitPlan(ctx context.Context, handoffDir, repoDir string, opts Options) (PlanCheckResult, error) {
	if opts.Policy.CommitPlan == "" {
		opts.Policy.CommitPlan = domain.CommitPlanPlanPreferred
	}
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return PlanCheckResult{}, err
	}
	if opts.Policy.CommitPlan != domain.CommitPlanPlanPreferred {
		return PlanCheckResult{}, fmt.Errorf("commit plan mode %q cannot be validated as honored: %w",
			opts.Policy.CommitPlan, ErrInvalidOptions)
	}
	in, err := runIntake(ctx, handoffDir, repoDir, opts, false)
	if err != nil {
		if isOperationalCheckError(ctx, err) {
			return PlanCheckResult{}, err
		}
		return PlanCheckResult{
			Honored: false,
			Problems: []PlanProblem{{
				Code:    PlanProblemHandoffRefused,
				Message: err.Error(),
			}},
		}, nil
	}
	defer in.close()

	var fatalProblems []PlanProblem
	for _, f := range in.findings {
		if !f.Fatal(opts.Policy.FindingProfile, opts.Policy.CandidateAutomationChanges) {
			continue
		}
		code := planProblemCodeForFinding(f.Kind)
		fatalProblems = append(fatalProblems, PlanProblem{
			Code:    code,
			Path:    f.Path,
			Message: code.description(),
		})
	}
	if len(fatalProblems) > 0 {
		return PlanCheckResult{Honored: false, Problems: fatalProblems}, nil
	}
	if len(in.changes) == 0 {
		if !in.planPresent {
			return PlanCheckResult{Honored: true, Problems: []PlanProblem{}}, nil
		}
		return PlanCheckResult{
			Honored: false,
			Problems: []PlanProblem{{
				Code:    PlanProblemNoChanges,
				Message: PlanProblemNoChanges.description(),
			}},
		}, nil
	}
	if !in.planPresent {
		return PlanCheckResult{
			Honored: false,
			Problems: []PlanProblem{{
				Code:    PlanProblemPlanMissing,
				Message: PlanProblemPlanMissing.description(),
			}},
		}, nil
	}
	dec, err := evaluateCommitPlan(in.planRaw, in.changes, in.base, opts.Policy)
	if err != nil {
		return PlanCheckResult{}, err
	}
	if dec.structuralErr != nil {
		problems, err := problemsForStructuralError(dec.structuralErr)
		if err != nil {
			return PlanCheckResult{}, err
		}
		return PlanCheckResult{
			Honored:  false,
			Problems: problems,
		}, nil
	}
	if len(dec.messageFindings) > 0 {
		problems := make([]PlanProblem, 0, len(dec.messageFindings))
		for _, mf := range dec.messageFindings {
			code := planProblemCodeForMessage(mf.Kind)
			problems = append(problems, PlanProblem{
				Code:    code,
				Group:   mf.GroupOrdinal,
				Message: code.description(),
			})
		}
		return PlanCheckResult{Honored: false, Problems: problems}, nil
	}
	return PlanCheckResult{Honored: true, Problems: []PlanProblem{}}, nil
}

// isOperationalCheckError separates errors that prevented the check from
// running (caller options, repository/base access, git plumbing, context
// cancellation) from handoff intake refusals, which become
// PlanProblemHandoffRefused.
func isOperationalCheckError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, ErrInvalidOptions) ||
		errors.Is(err, ErrBaseMismatch) ||
		errors.Is(err, ErrUnsupportedRepo) ||
		errors.Is(err, ErrGitPlumbing)
}

func problemsForStructuralError(err error) ([]PlanProblem, error) {
	var se *planStructuralError
	if !errors.As(err, &se) || !se.code.valid() {
		return nil, fmt.Errorf("commit plan structural error has no problem code: %w", err)
	}
	if len(se.paths) > 0 {
		problems := make([]PlanProblem, 0, len(se.paths)+1)
		for _, p := range se.paths {
			problems = append(problems, PlanProblem{
				Code:    se.code,
				Group:   se.group,
				Path:    p,
				Message: se.code.description(),
			})
		}
		if se.total > len(se.paths) {
			problems = append(problems, PlanProblem{
				Code:    se.code,
				Group:   se.group,
				Message: fmt.Sprintf("%d more changed paths are not covered by any commit plan group (cap %d)", se.total-len(se.paths), MaxReportedUncoveredPaths),
			})
		}
		return problems, nil
	}
	return []PlanProblem{{
		Code:    se.code,
		Group:   se.group,
		Path:    se.path,
		Message: se.code.description(),
	}}, nil
}
