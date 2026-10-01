package domain_test

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestRoleShapeAndPlace is the role map's fixture: the five execution roles
// are ward roles in their stage, every other role is wardless with no stage,
// and no site id is named by two roles.
func TestRoleShapeAndPlace(t *testing.T) {
	t.Parallel()
	wardStages := map[domain.RoleName]domain.StageName{
		domain.RoleSpecifier:      domain.StageNameSpecification,
		domain.RoleImplementer:    domain.StageNameImplementation,
		domain.RoleRemediator:     domain.StageNameImplementation,
		domain.RoleReviewer:       domain.StageNameReview,
		domain.RoleShadowReviewer: domain.StageNameReview,
	}
	if len(domain.AllRoleNames) != 13 {
		t.Fatalf("role list has %d members, want the 13 of plan revision 65", len(domain.AllRoleNames))
	}
	owners := map[string]domain.RoleName{}
	for _, role := range domain.AllRoleNames {
		stage, inStage := role.Stage()
		wantStage, ward := wardStages[role]
		if ward {
			if role.LaunchShape() != domain.LaunchShapeWard || !inStage || stage != wantStage {
				t.Fatalf("%s: shape %q, stage %q (%t), want ward in %q", role, role.LaunchShape(), stage, inStage, wantStage)
			}
			if len(role.Sites()) != 0 {
				t.Fatalf("ward role %s names sites %v", role, role.Sites())
			}
			continue
		}
		if role.LaunchShape() != domain.LaunchShapeWardless || inStage {
			t.Fatalf("%s: shape %q, stage %q (%t), want wardless with no stage", role, role.LaunchShape(), stage, inStage)
		}
		for _, site := range role.Sites() {
			if prior, taken := owners[site]; taken {
				t.Fatalf("site %q belongs to both %s and %s", site, prior, role)
			}
			owners[site] = role
			if got, ok := domain.RoleForSite(site); !ok || got != role {
				t.Fatalf("RoleForSite(%q) = %q, %t, want %s", site, got, ok, role)
			}
		}
	}
	if got := domain.RolePublicationAuthor.Sites(); !slices.Equal(got,
		[]string{"publication_author_explain", "publication_author_propose"}) {
		t.Fatalf("publication author sites = %v", got)
	}
	for _, unbuilt := range []domain.RoleName{domain.RoleDriftAuditor, domain.RoleBriefer} {
		if len(unbuilt.Sites()) != 0 {
			t.Fatalf("%s names sites %v before its site exists", unbuilt, unbuilt.Sites())
		}
	}
	for _, notRole := range []domain.RoleName{"", "verification", "researcher", "implementation"} {
		_, inStage := notRole.Stage()
		if notRole.LaunchShape() != "" || inStage || notRole.Sites() != nil {
			t.Fatalf("%q is not a role but has a shape or place", notRole)
		}
	}
	if role, ok := domain.RoleForSite("verification"); ok {
		t.Fatalf("RoleForSite(verification) = %s", role)
	}
}
