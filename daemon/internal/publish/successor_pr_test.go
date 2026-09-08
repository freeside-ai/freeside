package publish

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

func TestSuccessorPRRecoveryCoordinates(t *testing.T) {
	old := Identity{digest: domain.Digest("sha256:" + strings.Repeat("a", 64))}
	current := Identity{digest: domain.Digest("sha256:" + strings.Repeat("b", 64))}
	foreign := Identity{digest: domain.Digest("sha256:" + strings.Repeat("c", 64))}
	oldHead, newHead := strings.Repeat("1", 40), strings.Repeat("2", 40)
	candidate := Candidate{
		Repo: "owner/repo", BaseRef: "main", HeadSHA: newHead,
		Successor: &publicationrecord.SuccessorTarget{ItemID: "prior", Identity: old.Digest(), HeadSHA: oldHead, PRNumber: 108, Branch: "feat/retained"},
	}
	repo := repoRef{owner: "owner", name: "repo"}
	for markerName, marker := range map[string]Identity{"old": old, "new": current, "foreign": foreign} {
		for headName, head := range map[string]string{"old": oldHead, "new": newHead, "foreign": strings.Repeat("3", 40)} {
			t.Run(markerName+"-"+headName, func(t *testing.T) {
				pr := prState{
					Number: 108, State: "open", Body: marker.Marker(), HeadRef: "feat/retained", HeadSHA: head,
					HeadRepo: "owner/repo", BaseRef: "main", BaseRepo: "owner/repo",
				}
				_, err := validateSuccessorPR(pr, repo, current, candidate)
				allowed := markerName == "old" && headName != "foreign" || markerName == "new" && headName == "new"
				if (err == nil) != allowed {
					t.Fatalf("accepted=%v, want %v: %v", err == nil, allowed, err)
				}
				if !allowed {
					return
				}
				for _, corrupt := range []func(*prState){
					func(p *prState) { p.State = "closed" }, func(p *prState) { p.Number++ },
					func(p *prState) { p.HeadRepo = "foreign/repo" }, func(p *prState) { p.BaseRepo = "foreign/repo" },
					func(p *prState) { p.HeadRef = "feat/foreign" }, func(p *prState) { p.BaseRef = "other" },
				} {
					changed := pr
					corrupt(&changed)
					if _, err := validateSuccessorPR(changed, repo, current, candidate); err == nil {
						t.Fatalf("accepted changed PR: %#v", changed)
					}
				}
			})
		}
	}
}
