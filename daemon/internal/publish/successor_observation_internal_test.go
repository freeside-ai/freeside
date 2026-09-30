package publish

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

// A successor's reads of GitHub's PR view can lag the branch for a moment
// after a push (#1544). The two lagging shapes are pending, not conflicts;
// every other disagreement stays the immediate refusal it was.
func TestSuccessorPRObservationPending(t *testing.T) {
	ctx := context.Background()
	old := Identity{digest: domain.Digest("sha256:" + strings.Repeat("a", 64))}
	current := Identity{digest: domain.Digest("sha256:" + strings.Repeat("b", 64))}
	oldHead, newHead := strings.Repeat("1", 40), strings.Repeat("2", 40)
	const branch = "feat/retained"
	candidate := Candidate{
		Repo: "owner/name", BaseRef: "main", HeadSHA: newHead,
		Successor: &publicationrecord.SuccessorTarget{
			ItemID: "prior", Identity: old.Digest(), HeadSHA: oldHead, PRNumber: 108, Branch: branch,
		},
	}
	seed := func(g *draftFakeGitHub, number int, state, body, head string) {
		g.prs[number] = &draftFakePR{
			number: number, state: state, title: "Title", body: body,
			headRef: branch, headSHA: head, baseRef: "main",
		}
	}

	for _, tc := range []struct {
		name     string
		seed     func(*draftFakeGitHub)
		converge bool
		want     error
	}{
		{name: "empty listing", seed: func(*draftFakeGitHub) {}, want: ErrSuccessorPRNotListed},
		{name: "empty listing after push", seed: func(*draftFakeGitHub) {}, converge: true, want: ErrSuccessorPRNotListed},
		{
			name: "predecessor head after push", converge: true, want: ErrSuccessorPRHeadLagging,
			seed: func(g *draftFakeGitHub) { seed(g, 108, "open", old.Marker(), oldHead) },
		},
		{name: "two PRs", want: ErrPublicationConflict, seed: func(g *draftFakeGitHub) {
			seed(g, 108, "open", old.Marker(), oldHead)
			seed(g, 109, "open", old.Marker(), oldHead)
		}},
		{name: "foreign marker", converge: true, want: ErrForeignResource, seed: func(g *draftFakeGitHub) {
			seed(g, 108, "open", "no marker", oldHead)
		}},
		{
			name: "closed at predecessor head", converge: true, want: ErrPublicationConflict,
			seed: func(g *draftFakeGitHub) { seed(g, 108, "closed", old.Marker(), oldHead) },
		},
		{
			name: "another PR number at predecessor head", converge: true, want: ErrPublicationConflict,
			seed: func(g *draftFakeGitHub) { seed(g, 110, "open", old.Marker(), oldHead) },
		},
		{
			name: "neither head", converge: true, want: ErrPublicationConflict,
			seed: func(g *draftFakeGitHub) { seed(g, 108, "open", old.Marker(), strings.Repeat("3", 40)) },
		},
		{
			name: "successor marker at predecessor head", converge: true, want: ErrPublicationConflict,
			seed: func(g *draftFakeGitHub) { seed(g, 108, "open", current.Marker(), oldHead) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, g, repo := newDraftFake(t)
			tc.seed(g)
			p := &Publisher{forge: f}
			var err error
			if tc.converge {
				_, err = p.convergeSuccessorPR(ctx, repo, current, candidate, "Title", current.Marker())
			} else {
				_, err = p.observeSuccessorPR(ctx, repo, current, candidate)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			pending := errors.Is(err, ErrSuccessorObservationPending)
			if wantPending := errors.Is(tc.want, ErrSuccessorObservationPending); pending != wantPending {
				t.Fatalf("pending = %v, want %v: %v", pending, wantPending, err)
			}
			if pending && errors.Is(err, ErrPublicationConflict) {
				t.Fatalf("pending observation also reads as a conflict: %v", err)
			}
			if slices.ContainsFunc(g.requests, func(r string) bool { return strings.HasPrefix(r, http.MethodPatch) }) {
				t.Fatalf("a refused observation edited the PR: %v", g.requests)
			}
		})
	}
}
