package inference_test

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// Every role with a built site has exactly one prompt source: the daemon's,
// or the operator's file for the publication author. A role with no site has
// neither.
func TestRolePromptSources(t *testing.T) {
	for _, role := range domain.AllRoleNames {
		prompt, owned := inference.CodeOwnedRolePrompt(role)
		want := len(role.Sites()) > 0 && role != domain.RolePublicationAuthor
		if owned != want {
			t.Fatalf("CodeOwnedRolePrompt(%s) owned = %v, want %v", role, owned, want)
		}
		if !owned {
			continue
		}
		if prompt.Name != string(role)+"_v1" || len(prompt.Body) != 0 ||
			string(prompt.Digest) != contentaddr.Sum(nil) {
			t.Fatalf("CodeOwnedRolePrompt(%s) = %+v", role, prompt)
		}
	}
	first := inference.OperatorRolePrompt(domain.RolePublicationAuthor, []byte("one"))
	second := inference.OperatorRolePrompt(domain.RolePublicationAuthor, []byte("two"))
	if first.Name != "publication_author" || first.Name != second.Name || first.Digest == second.Digest ||
		string(first.Digest) != contentaddr.Sum([]byte("one")) {
		t.Fatalf("operator prompts = %+v, %+v", first, second)
	}
}
