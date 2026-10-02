package agenttree_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	agentName = "sol-via-codex"
	routeName = "openai_chatgpt_codex"
)

func digestOf(text string) domain.Digest { return domain.Digest(contentaddr.Sum([]byte(text))) }

// must unwraps a fixture step that cannot fail on a valid fixture; a panic
// fails the test with the step's error.
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// fixtureTree is one agent with its three fragments, a reviewer lineup line
// that selects it, and one attended mark.
func fixtureTree(t *testing.T) agenttree.Tree {
	t.Helper()
	route := domain.RouteFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		ServiceOperator: "openai", Protocol: "chatgpt_backend",
		InferenceAuthorities: []string{"chatgpt.com"},
		BillingMode:          "subscription", FallbackPolicy: "fail_closed",
		TermsBasisDate: "2026-08-22",
	}
	route.Digest = must(route.ComputeDigest())
	adapter := domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    "codex_proto_v1@build-7", HarnessBuild: "codex-cli 0.29.0",
		ClientKind: domain.HarnessClientCodexCLI, Vendor: domain.AgentVendorCodex,
		LaunchCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapReadTools, domain.LaunchCapInstructionDelivery,
			domain.LaunchCapStructuredOutput, domain.LaunchCapRouteStoreContract,
		),
		SendableEfforts: []domain.EffortLevel{domain.EffortHigh, domain.EffortMax},
	}
	adapter.Digest = must(adapter.ComputeDigest())
	offer := domain.OfferFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		RouteModelID:    "gpt-5.6-sol", LineageGroup: "openai",
		IdentityStability: domain.IdentityPinned,
		AllowedEfforts:    []domain.EffortLevel{domain.EffortHigh, domain.EffortMax},
		PricingRevision:   "2026-08",
		NotAfter:          time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	offer.Digest = must(offer.ComputeDigest())
	tree := agenttree.Tree{
		Agents: []domain.AgentSource{{
			Name: agentName, Enrollment: "openai-chatgpt-a/codex_cli",
			Route: routeName, Adapter: "codex_proto_v1", Offer: "gpt-5.6-sol", Effort: domain.EffortMax,
		}},
		Routes:   []agenttree.Route{{Name: routeName, Fragment: route}},
		Adapters: []agenttree.Adapter{{Name: "codex_proto_v1", Fragment: adapter}},
		Offers:   []agenttree.Offer{{Route: routeName, Name: "gpt-5.6-sol", Fragment: offer}},
	}
	agentDigest := must(tree.AgentDigest(agentName))
	tree.Marks = []agenttree.Mark{{Agent: agentName, AgentDigest: agentDigest, LaunchDigest: digestOf("launch")}}
	tree.Lineup = []agenttree.LineupLine{{
		Key: string(domain.RoleReviewer),
		Selection: domain.LineupSelection{
			AgentName: agentName, AgentDigest: agentDigest,
			PromptName: "review", PromptDigest: digestOf("prompt"),
		},
	}}
	return tree
}

func fixtureRecords() (domain.ClientEnrollment, domain.AuthIdentity) {
	return domain.ClientEnrollment{
			ID: "openai-chatgpt-a/codex_cli", AuthIdentityID: "openai-chatgpt-a",
			HarnessClient: domain.HarnessClientCodexCLI, Route: routeName,
			AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
			RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
			AccountBinding: "acct-1",
		}, domain.AuthIdentity{
			ID: "openai-chatgpt-a", Provider: "codex", AuthStoreMutationLease: true,
			MaxParallelExecutions: 1, Enabled: true, AccountBinding: "acct-1", CostOwner: "owner",
			Interim: domain.InterimClientFacts{AuthStoreVolume: "codex-store", RefreshStrategy: domain.RefreshOnDemand},
		}
}

func TestRenderParseRoundTrip(t *testing.T) {
	tree := fixtureTree(t)
	files := must(agenttree.Render(tree))
	parsed := must(agenttree.Parse(files))
	if !reflect.DeepEqual(parsed, tree) {
		t.Fatalf("Parse(Render(tree)) =\n%+v\nwant\n%+v", parsed, tree)
	}
	again := must(agenttree.Render(parsed))
	if !reflect.DeepEqual(again, files) {
		t.Fatal("rendering the parsed tree changed its bytes")
	}
	want := "who      enrollment  openai-chatgpt-a/codex_cli\n" +
		"through  route       openai_chatgpt_codex\n" +
		"running  adapter     codex_proto_v1\n" +
		"asking   offer       gpt-5.6-sol, effort max\n"
	if got := string(files["agents/"+agentName]); got != want {
		t.Fatalf("agent document =\n%s\nwant\n%s", got, want)
	}
	if lock := string(files["agents.lock"]); strings.Count(lock, "\n") != 4 ||
		!strings.Contains(lock, "offer "+routeName+"/gpt-5.6-sol sha256:") {
		t.Fatalf("agents.lock =\n%s", lock)
	}
}

func TestAgentDocumentToleratesSpacing(t *testing.T) {
	files := must(agenttree.Render(fixtureTree(t)))
	files["agents/"+agentName] = []byte("who enrollment openai-chatgpt-a/codex_cli\n" +
		"through route openai_chatgpt_codex\nrunning\tadapter codex_proto_v1\n" +
		"asking offer gpt-5.6-sol,  effort max\n")
	if _, err := agenttree.Parse(files); err != nil {
		t.Fatal(err)
	}
}

func TestParseFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(agenttree.Files)
		want   error
	}{
		{"file outside the layout", func(f agenttree.Files) { f["notes"] = []byte("x\n") }, agenttree.ErrMalformed},
		{"no lineup", func(f agenttree.Files) { delete(f, "lineup") }, agenttree.ErrMalformed},
		{"no lock", func(f agenttree.Files) { delete(f, "agents.lock") }, agenttree.ErrMalformed},
		{"missing fragment", func(f agenttree.Files) {
			delete(f, "fragments/routes/"+routeName)
		}, agenttree.ErrMalformed},
		{"offer under another route", func(f agenttree.Files) {
			f["fragments/offers/other/gpt-5.6-sol"] = f["fragments/offers/"+routeName+"/gpt-5.6-sol"]
			delete(f, "fragments/offers/"+routeName+"/gpt-5.6-sol")
		}, agenttree.ErrMalformed},
		{"agent document with a fifth line", func(f agenttree.Files) {
			f["agents/"+agentName] = append(f["agents/"+agentName], "# note\n"...)
		}, agenttree.ErrMalformed},
		{"agent document with lines reordered", func(f agenttree.Files) {
			lines := strings.SplitAfter(string(f["agents/"+agentName]), "\n")
			f["agents/"+agentName] = []byte(lines[1] + lines[0] + lines[2] + lines[3])
		}, agenttree.ErrMalformed},
		{"unknown effort", func(f agenttree.Files) {
			f["agents/"+agentName] = bytes.Replace(f["agents/"+agentName], []byte("effort max"), []byte("effort huge"), 1)
		}, domain.ErrInvalidEffortLevel},
		{"agent name outside the rule", func(f agenttree.Files) {
			f["agents/Sol"] = f["agents/"+agentName]
		}, domain.ErrInvalidAgentName},
		{"fragment with an unknown field", func(f agenttree.Files) {
			f["fragments/routes/"+routeName] = bytes.Replace(
				f["fragments/routes/"+routeName], []byte("{"), []byte(`{"note":"x",`), 1)
		}, agenttree.ErrMalformed},
		{"fragment whose digest is of other content", func(f agenttree.Files) {
			f["fragments/routes/"+routeName] = bytes.Replace(
				f["fragments/routes/"+routeName], []byte("2026-08-22"), []byte("2026-08-23"), 1)
		}, domain.ErrAgentDigestMismatch},
		{"fragment without a final newline", func(f agenttree.Files) {
			body := f["fragments/routes/"+routeName]
			f["fragments/routes/"+routeName] = body[:len(body)-1]
		}, agenttree.ErrMalformed},
		{"mark for an agent the tree lacks", func(f agenttree.Files) {
			f["agents/ghost.attended"] = f["agents/"+agentName+".attended"]
		}, agenttree.ErrUnknownAgent},
		{"mark that is not two digests", func(f agenttree.Files) {
			f["agents/"+agentName+".attended"] = []byte("yes\n")
		}, agenttree.ErrMalformed},
		{"lineup role outside the vocabulary", func(f agenttree.Files) {
			f["lineup"] = bytes.Replace(f["lineup"], []byte("reviewer"), []byte("critic"), 1)
		}, agenttree.ErrMalformed},
		{"lineup line without a prompt", func(f agenttree.Files) {
			f["lineup"] = []byte("reviewer " + agentName + "@" + string(digestOf("a")) + "\n")
		}, domain.ErrInvalidLineupKey},
		{"lock missing an entry", func(f agenttree.Files) {
			f["agents.lock"] = f["agents.lock"][bytes.IndexByte(f["agents.lock"], '\n')+1:]
		}, agenttree.ErrLockMismatch},
		{"lock naming another digest", func(f agenttree.Files) {
			lines := strings.SplitAfter(string(f["agents.lock"]), "\n")
			fields := strings.Fields(lines[1])
			lines[1] = fields[0] + " " + fields[1] + " " + string(digestOf("other")) + "\n"
			f["agents.lock"] = []byte(strings.Join(lines, ""))
		}, agenttree.ErrLockMismatch},
		{"oversized file", func(f agenttree.Files) {
			f["lineup"] = bytes.Repeat([]byte("x"), domain.MaxAgentFragmentBytes+1)
		}, agenttree.ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := must(agenttree.Render(fixtureTree(t)))
			tc.mutate(files)
			if _, err := agenttree.Parse(files); !errors.Is(err, tc.want) {
				t.Fatalf("Parse = %v, want %v", err, tc.want)
			}
		})
	}
}

// An edit to a fragment changes the agent's digest, so the lock, which still
// names the old digests, stops the load: nobody admits against content the
// lock's reviewer did not see.
func TestFragmentEditWithoutLockUpdateFails(t *testing.T) {
	tree := fixtureTree(t)
	files := must(agenttree.Render(tree))
	tree.Routes[0].Fragment.TermsBasisDate = "2026-09-01"
	tree.Routes[0].Fragment.Digest = must(tree.Routes[0].Fragment.ComputeDigest())
	edited := must(tree.Routes[0].Fragment.Encode())
	files["fragments/routes/"+routeName] = append(edited, '\n')
	if _, err := agenttree.Parse(files); !errors.Is(err, agenttree.ErrLockMismatch) {
		t.Fatalf("Parse = %v, want %v", err, agenttree.ErrLockMismatch)
	}
}

func TestAttendedNeedsTheExactMark(t *testing.T) {
	tree := fixtureTree(t)
	mark := tree.Marks[0]
	if tree.Attended(agentName, mark.AgentDigest, mark.LaunchDigest) {
		t.Fatal("marked agent and launch still attended")
	}
	if !tree.Attended(agentName, mark.AgentDigest, digestOf("another launch")) {
		t.Fatal("mark carried to another launch")
	}
	if !tree.Attended(agentName, digestOf("edited agent"), mark.LaunchDigest) {
		t.Fatal("mark carried to another agent digest")
	}
	if !tree.Attended("other", mark.AgentDigest, mark.LaunchDigest) {
		t.Fatal("mark carried to another agent name")
	}
}

func TestResolveAgent(t *testing.T) {
	tree := fixtureTree(t)
	enrollment, identity := fixtureRecords()
	resolved := must(tree.ResolveAgent(agentName, enrollment, identity))
	if want := must(tree.AgentDigest(agentName)); resolved.Definition.Digest != want {
		t.Fatalf("resolved digest %s, tree digest %s", resolved.Definition.Digest, want)
	}
	if resolved.Offer.RouteModelID != "gpt-5.6-sol" {
		t.Fatalf("resolved offer = %+v", resolved.Offer)
	}

	if _, err := tree.ResolveAgent("ghost", enrollment, identity); !errors.Is(err, agenttree.ErrUnknownAgent) {
		t.Fatalf("unknown agent = %v", err)
	}
	other := enrollment
	other.ID = "openai-chatgpt-b/codex_cli"
	if _, err := tree.ResolveAgent(agentName, other, identity); !errors.Is(err, domain.ErrAgentJoinInvalid) {
		t.Fatalf("another enrollment = %v", err)
	}
	disabled := identity
	disabled.Enabled = false
	if _, err := tree.ResolveAgent(agentName, enrollment, disabled); !errors.Is(err, domain.ErrAgentJoinInvalid) {
		t.Fatalf("disabled identity = %v", err)
	}
}

func TestRevisionCoversContentNotOrder(t *testing.T) {
	files := must(agenttree.Render(fixtureTree(t)))
	first := must(agenttree.Revision(files))
	if again := must(agenttree.Revision(files)); again != first {
		t.Fatal("revision is not stable")
	}
	files["lineup"] = append(files["lineup"], '\n')
	if changed := must(agenttree.Revision(files)); changed == first {
		t.Fatal("revision ignored a content change")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture running git over test-owned repos with test-chosen arguments
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func applyAndCommit(t *testing.T, repo string, patch []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tree.patch")
	if err := os.WriteFile(path, patch, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "apply", "--index", path)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "tree")
	return git(t, repo, "rev-parse", "HEAD")
}

func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	if err := os.MkdirAll(filepath.Join(repo, "policy"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "policy", "README.md"), []byte("# Policy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")
	return repo
}

// The patch is what a human commits, so the proof is end to end: apply it
// with git, read the commit back, and get the tree that was rendered.
func TestPatchAppliesAndReadsBackAtTheCommit(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	tree := fixtureTree(t)
	files := must(agenttree.Render(tree))
	commit := applyAndCommit(t, repo, must(agenttree.Patch(nil, files)))

	read := must(agenttree.ReadCommit(ctx, repo, t.TempDir(), commit))
	if !reflect.DeepEqual(read, files) {
		t.Fatalf("ReadCommit = %v, want the rendered files", read)
	}
	if parsed := must(agenttree.Parse(read)); !reflect.DeepEqual(parsed, tree) {
		t.Fatal("tree read at the commit differs from the rendered tree")
	}

	// A second revision: one file changes, one goes, one arrives.
	next := tree
	next.Marks = nil
	next.Agents = append([]domain.AgentSource{{
		Name: "max-via-codex", Enrollment: "openai-chatgpt-a/codex_cli",
		Route: routeName, Adapter: "codex_proto_v1", Offer: "gpt-5.6-sol", Effort: domain.EffortHigh,
	}}, tree.Agents...)
	nextFiles := must(agenttree.Render(next))
	second := applyAndCommit(t, repo, must(agenttree.Patch(files, nextFiles)))
	if read := must(agenttree.ReadCommit(ctx, repo, t.TempDir(), second)); !reflect.DeepEqual(read, nextFiles) {
		t.Fatal("second revision read back differs from its rendered files")
	}
	// The first commit still reads as the first revision.
	if read := must(agenttree.ReadCommit(ctx, repo, t.TempDir(), commit)); !reflect.DeepEqual(read, files) {
		t.Fatal("first revision changed after a later commit")
	}
	if patch := must(agenttree.Patch(nextFiles, nextFiles)); len(patch) != 0 {
		t.Fatalf("patch between equal revisions = %q", patch)
	}
}

func TestReadCommitIgnoresTheWorkingTree(t *testing.T) {
	repo := newRepo(t)
	files := must(agenttree.Render(fixtureTree(t)))
	commit := applyAndCommit(t, repo, must(agenttree.Patch(nil, files)))
	if err := os.WriteFile(filepath.Join(repo, "policy", "lineup"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	read := must(agenttree.ReadCommit(context.Background(), repo, t.TempDir(), commit))
	if !bytes.Equal(read["lineup"], files["lineup"]) {
		t.Fatal("an uncommitted edit reached the read")
	}
}

func TestReadCommitRefusals(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	files := must(agenttree.Render(fixtureTree(t)))
	commit := applyAndCommit(t, repo, must(agenttree.Patch(nil, files)))
	git(t, repo, "tag", "-a", "v1", "-m", "v1")

	for name, source := range map[string]string{
		"branch name":  "HEAD",
		"abbreviation": commit[:12],
		"tag object":   git(t, repo, "rev-parse", "v1"),
		"tree object":  git(t, repo, "rev-parse", commit+"^{tree}"),
		"absent":       strings.Repeat("0", len(commit)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := agenttree.ReadCommit(ctx, repo, t.TempDir(), source); !errors.Is(err, agenttree.ErrCommit) {
				t.Fatalf("ReadCommit(%s) = %v, want %v", source, err, agenttree.ErrCommit)
			}
		})
	}

	t.Run("symlink in the tree", func(t *testing.T) {
		if err := os.Symlink("../README.md", filepath.Join(repo, "policy", "agents", "linked")); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", ".")
		git(t, repo, "commit", "-q", "-m", "link")
		linked := git(t, repo, "rev-parse", "HEAD")
		if _, err := agenttree.ReadCommit(ctx, repo, t.TempDir(), linked); !errors.Is(err, agenttree.ErrMalformed) {
			t.Fatalf("ReadCommit = %v, want %v", err, agenttree.ErrMalformed)
		}
	})
}
