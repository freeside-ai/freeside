package domain

import (
	"fmt"
	"slices"
)

// Each role's launch shape and its place: the stage a ward role runs in, or
// the §5.13 sites a wardless role answers (plan §5.4, Roles and Launch
// Shapes). A stage may hold several roles and a role may span several sites.
// The switches below dispatch on the role and omit default, so the
// exhaustive linter makes a new role's author decide both.

// LaunchShape returns the role's launch shape. An invalid role has none, so
// a caller comparing against LaunchShapeWardless fails closed.
func (r RoleName) LaunchShape() LaunchShape {
	switch r {
	case RoleSpecifier, RoleImplementer, RoleRemediator, RoleReviewer, RoleShadowReviewer:
		return LaunchShapeWard
	case RoleDiagnostic, RoleTaskNamer, RolePublicationAuthor, RoleFindingClassifier,
		RoleFindingAdjudicator, RoleDriftAuditor, RoleAttentionDiscussion, RoleBriefer:
		return LaunchShapeWardless
	}
	return ""
}

// Stage returns the stage a ward role runs in. A wardless role runs in no
// stage and reports false.
func (r RoleName) Stage() (StageName, bool) {
	switch r {
	case RoleSpecifier:
		return StageNameSpecification, true
	case RoleImplementer, RoleRemediator:
		return StageNameImplementation, true
	case RoleReviewer, RoleShadowReviewer:
		return StageNameReview, true
	case RoleDiagnostic, RoleTaskNamer, RolePublicationAuthor, RoleFindingClassifier,
		RoleFindingAdjudicator, RoleDriftAuditor, RoleAttentionDiscussion, RoleBriefer:
		return "", false
	}
	return "", false
}

// Sites returns the ids of the judgment sites a wardless role answers, as
// daemon/internal/inference registers them; domain cannot import that
// package, and a test there holds the two lists together. A ward role has
// none. The briefer's site is not built, so that role has none yet and the unit
// that builds the site adds it.
func (r RoleName) Sites() []string {
	switch r {
	case RoleDiagnostic:
		return []string{"execution_diagnostic"}
	case RoleTaskNamer:
		return []string{"task_namer"}
	case RolePublicationAuthor:
		return []string{"publication_author_explain", "publication_author_propose"}
	case RoleFindingClassifier:
		return []string{"finding_classifier"}
	case RoleFindingAdjudicator:
		return []string{"finding_adjudicator"}
	case RoleAttentionDiscussion:
		return []string{"attention_discussion"}
	case RoleDriftAuditor:
		return []string{"drift_auditor"}
	case RoleBriefer:
		return nil
	case RoleSpecifier, RoleImplementer, RoleRemediator, RoleReviewer, RoleShadowReviewer:
		return nil
	}
	return nil
}

// RoleForSite returns the one role a judgment site belongs to (§5.13: every
// site belongs to exactly one role). An id no role names reports false.
func RoleForSite(site string) (RoleName, bool) {
	for _, role := range AllRoleNames {
		if slices.Contains(role.Sites(), site) {
			return role, true
		}
	}
	return "", false
}

// WritingRoles are the roles whose work a judging role judges (plan §7,
// Review Independence).
var WritingRoles = []RoleName{RoleImplementer, RoleRemediator}

// JudgesWrittenWork reports whether the role is a judging role (§7): its
// record says, for each writing role, whether the two shared a lineage group.
func (r RoleName) JudgesWrittenWork() bool {
	switch r {
	case RoleReviewer, RoleFindingAdjudicator, RoleDriftAuditor:
		return true
	case RoleSpecifier, RoleImplementer, RoleRemediator, RoleShadowReviewer,
		RoleDiagnostic, RoleTaskNamer, RolePublicationAuthor, RoleFindingClassifier,
		RoleAttentionDiscussion, RoleBriefer:
		return false
	}
	return false
}

// LineageRelation is what a record says about a judging role's lineage group
// against one writing role's (§7). It is a recorded fact and never a gate
// (plan revision 65).
type LineageRelation string

const (
	LineageMatched  LineageRelation = "matched"
	LineageDiffered LineageRelation = "differed"
	// LineageUnknown is recorded when either offer carries no lineage group,
	// or the writing role has no line to read one from.
	LineageUnknown LineageRelation = "unknown"
)

// AllLineageRelations lists every valid LineageRelation.
var AllLineageRelations = []LineageRelation{LineageMatched, LineageDiffered, LineageUnknown}

func (r LineageRelation) valid() bool {
	switch r {
	case LineageMatched, LineageDiffered, LineageUnknown:
		return true
	default:
		return false
	}
}

// Validate reports whether the relation is a member of the closed list.
func (r LineageRelation) Validate() error {
	if !r.valid() {
		return fmt.Errorf("lineage relation %q: %w", r, ErrInvalidLineageRelation)
	}
	return nil
}

// CompareLineage relates a judging role's lineage group to a writing role's.
// An empty group on either side is unknown, never a difference.
func CompareLineage(judging, writing string) LineageRelation {
	switch {
	case judging == "" || writing == "":
		return LineageUnknown
	case judging == writing:
		return LineageMatched
	default:
		return LineageDiffered
	}
}
