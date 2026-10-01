package domain

import "slices"

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
// none. The drift auditor's site (#1050) and the briefer's are not built, so
// those roles have none yet and the units that build the sites add them.
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
	case RoleDriftAuditor, RoleBriefer:
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
