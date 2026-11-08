package agenttree_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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
	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
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

// fixtureTree is one agent with its three fragments and a reviewer lineup
// line that selects it.
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
	tree.Lineup = []agenttree.LineupLine{{
		Key: string(domain.RoleReviewer),
		Selection: domain.LineupSelection{
			Names:      namesOf(must(tree.SelectionOfAgent(agentName))),
			PromptName: "review", PromptDigest: digestOf("prompt"),
		},
	}}
	return tree
}

func namesOf(s domain.TaskLineSelection) domain.AgentSelectionNames {
	return domain.AgentSelectionNames{Enrollment: string(s.EnrollmentID), Model: s.ModelID, Effort: s.Effort}
}

func fixtureRecords() (domain.ClientEnrollment, domain.AuthIdentity) {
	return domain.ClientEnrollment{
		ID: "openai-chatgpt-a/codex_cli", AuthIdentityID: "openai-chatgpt-a",
		HarnessClient: domain.HarnessClientCodexCLI, Route: routeName,
		AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: "acct-1",
	}, domain.AuthIdentity{
		ID: "openai-chatgpt-a", Provider: domain.AuthProviderOpenAI, AuthStoreMutationLease: true,
		Enabled: true, AccountBinding: "acct-1", CostOwner: "owner",
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
		{"lineup role outside the vocabulary", func(f agenttree.Files) {
			f["lineup"] = bytes.Replace(f["lineup"], []byte("reviewer"), []byte("critic"), 1)
		}, agenttree.ErrMalformed},
		{"lineup line without a prompt", func(f agenttree.Files) {
			f["lineup"] = []byte("reviewer openai-chatgpt-a/codex_cli/gpt-5.6-sol/xhigh\n")
		}, domain.ErrInvalidLineupKey},
		{"lineup line naming an agent", func(f agenttree.Files) {
			f["lineup"] = []byte("reviewer " + agentName + "@" + string(digestOf("a")) + "/review@" + string(digestOf("p")) + "\n")
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

// escalatedTree adds to the fixture a specifier line on the fixture
// agent's selection and the escalated line under keyEnrollment's key that
// escalates value to effort.
func escalatedTree(t *testing.T, keyEnrollment string, value domain.AgentSelectionNames) agenttree.Tree {
	t.Helper()
	tree := fixtureTree(t)
	specifier := domain.LineupSelection{
		Names:      namesOf(must(tree.SelectionOfAgent(agentName))),
		PromptName: "specifier", PromptDigest: digestOf("specifier"),
	}
	escalated := specifier
	escalated.Names = value
	tree.Lineup = append(tree.Lineup,
		agenttree.LineupLine{Key: string(domain.RoleSpecifier), Selection: specifier},
		agenttree.LineupLine{Key: "specifier.escalated." + keyEnrollment, Selection: escalated},
	)
	return tree
}

// An escalated line survives the round trip when it escalates its key's
// enrollment to another effort on that enrollment, and the tree refuses it,
// with no agent read, when its value runs another enrollment, whether it is
// rendered that way or the lineup file is edited to say so, or when it
// repeats the specifier line.
func TestEscalatedLineFollowsThePairingRule(t *testing.T) {
	const enrollment = "openai-chatgpt-a/codex_cli"
	high := domain.AgentSelectionNames{Enrollment: enrollment, Model: "gpt-5.6-sol", Effort: "high"}
	tree := escalatedTree(t, enrollment, high)
	files := must(agenttree.Render(tree))
	parsed := must(agenttree.Parse(files))
	if !reflect.DeepEqual(parsed, tree) {
		t.Fatalf("Parse(Render(tree)) =\n%+v\nwant\n%+v", parsed, tree)
	}
	lineup := must(parsed.ResolveLineup())
	if counterpart, ok := lineup.EscalatedCounterpart(domain.RoleSpecifier, enrollment); !ok || counterpart.Names != high {
		t.Fatalf("EscalatedCounterpart = %+v, %t", counterpart, ok)
	}

	elsewhere := high
	elsewhere.Enrollment = "openai-chatgpt-b/codex_cli"
	if _, err := agenttree.Render(escalatedTree(t, enrollment, elsewhere)); !errors.Is(err, domain.ErrLineupEscalatedPair) {
		t.Fatalf("Render with a value on another enrollment = %v", err)
	}
	own := namesOf(must(tree.SelectionOfAgent(agentName)))
	if _, err := agenttree.Render(escalatedTree(t, enrollment, own)); !errors.Is(err, domain.ErrLineupEscalatedPair) {
		t.Fatalf("Render with the specifier's own line = %v", err)
	}

	edited := must(agenttree.Render(tree))
	edited["lineup"] = bytes.Replace(edited["lineup"],
		[]byte(" "+enrollment+"/gpt-5.6-sol/high/"), []byte(" "+elsewhere.Enrollment+"/gpt-5.6-sol/high/"), 1)
	_, err := agenttree.Parse(edited)
	if !errors.Is(err, domain.ErrLineupEscalatedPair) || !errors.Is(err, agenttree.ErrMalformed) {
		t.Fatalf("Parse with the value edited onto another enrollment = %v", err)
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

// TestLeftoverAttendedMarkIsRefused: plan revision 97 removed the attended
// mark, so a committed tree that still has an agent's .attended file is
// refused with the file named and the fix, not read past or reported as an
// unknown agent.
func TestLeftoverAttendedMarkIsRefused(t *testing.T) {
	files := must(agenttree.Render(fixtureTree(t)))
	path := "agents/" + agentName + ".attended"
	if _, ok := files[path]; ok {
		t.Fatalf("Render wrote %s", path)
	}
	files[path] = []byte(string(digestOf("agent")) + " " + string(digestOf("launch")) + "\n")
	_, err := agenttree.Parse(files)
	if !errors.Is(err, agenttree.ErrMalformed) || !strings.Contains(err.Error(), path) ||
		!strings.Contains(err.Error(), "delete the file") {
		t.Fatalf("Parse = %v, want %v naming %s and its removal", err, agenttree.ErrMalformed, path)
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
	// The agent's selection: its offer's model and the effort its adapter
	// sends, which for Codex's max is xhigh.
	if want := must(domain.ReduceAgentSelection(resolved.Definition, resolved.Adapter, resolved.Offer)); resolved.Selection != want {
		t.Fatalf("resolved selection %+v, the agent reduces to %+v", resolved.Selection, want)
	}
	if resolved.Selection.ModelID != "gpt-5.6-sol" || resolved.Selection.Effort != "xhigh" {
		t.Fatalf("resolved selection = %+v", resolved.Selection)
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

func TestAdapterForLaunch(t *testing.T) {
	tree := fixtureTree(t)
	launch := func(writer bool) domain.LaunchSpec {
		l := domain.LaunchSpec{
			EncodingVersion: domain.LaunchEncodingVersion, Stage: domain.StageNameReview, Writer: writer,
			OutputContract: "review-findings/v3", SessionMode: domain.SessionOneShot,
			AuxiliaryInference: domain.AuxiliaryForbidden,
		}
		l.Digest = must(l.ComputeDigest())
		return l
	}
	review := launch(false)
	got, err := tree.AdapterForLaunch(domain.HarnessClientCodexCLI, review, "high", domain.LaunchCapAuxiliaryInferenceControl)
	if err != nil || got.Digest != tree.Adapters[0].Fragment.Digest {
		t.Fatalf("review launch = %s %v, want the tree's codex adapter", got.AdapterBuild, err)
	}
	if _, err := tree.AdapterForLaunch(domain.HarnessClientCodexCLI, review, "high"); !errors.Is(err, domain.ErrNoAdapterForLaunch) {
		t.Fatalf("review launch, nothing waived = %v, want ErrNoAdapterForLaunch", err)
	}
	if _, err := tree.AdapterForLaunch(
		domain.HarnessClientClaudeCode, review, "high", domain.LaunchCapAuxiliaryInferenceControl,
	); !errors.Is(err, domain.ErrNoAdapterForLaunch) {
		t.Fatalf("a client kind the tree has no adapter for = %v, want ErrNoAdapterForLaunch", err)
	}
	if _, err := tree.AdapterForLaunch(
		domain.HarnessClientCodexCLI, launch(true), "high", domain.LaunchCapAuxiliaryInferenceControl,
	); !errors.Is(err, domain.ErrNoAdapterForLaunch) {
		t.Fatalf("a writer launch on a read-only adapter = %v, want ErrNoAdapterForLaunch", err)
	}
	second := tree.Adapters[0]
	second.Name = "codex_proto_v2"
	second.Fragment.AdapterBuild = "codex_proto_v2@build-1"
	second.Fragment.Digest = must(second.Fragment.ComputeDigest())
	tree.Adapters = append(tree.Adapters, second)
	if _, err := tree.AdapterForLaunch(
		domain.HarnessClientCodexCLI, review, "high", domain.LaunchCapAuxiliaryInferenceControl,
	); !errors.Is(err, domain.ErrAmbiguousAdapterForLaunch) {
		t.Fatalf("two covering codex adapters = %v, want ErrAmbiguousAdapterForLaunch", err)
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
	cmd.Env = append(gitenv.Scrubbed(),
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

	// A second revision: the agent is renamed, so its file goes, another
	// arrives, and the lineup changes.
	next := tree
	renamed := tree.Agents[0]
	renamed.Name = "max-via-codex"
	next.Agents = []domain.AgentSource{renamed}
	next.Lineup = []agenttree.LineupLine{tree.Lineup[0]}
	next.Lineup[0].Selection.PromptName = "renamed-review"
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

func gitWithInput(t *testing.T, dir string, input []byte, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture running git over test-owned repos with test-chosen arguments
	cmd.Env = append(gitenv.Scrubbed(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// The read starts one cat-file for the whole tree, not one per file.
func TestReadCommitStartsOneCatFile(t *testing.T) {
	repo := newRepo(t)
	const count = 60
	want := agenttree.Files{}
	for i := range count {
		rel := fmt.Sprintf("fragments/extra-%02d", i)
		want[rel] = fmt.Appendf(nil, "fragment %d\n", i)
		if err := os.MkdirAll(filepath.Join(repo, "policy", "fragments"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "policy", rel), want[rel], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "fragments")
	commit := git(t, repo, "rev-parse", "HEAD")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> '%s'\nexec '%s' \"$@\"\n", log, realGit)
	execfixture.Write(t, filepath.Join(bin, "git"), []byte(script), 0o700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	read := must(agenttree.ReadCommit(t.Context(), repo, t.TempDir(), commit))
	if !reflect.DeepEqual(read, want) {
		t.Fatalf("ReadCommit read %d files, want the %d committed", len(read), len(want))
	}
	calls, err := os.ReadFile(log) //nolint:gosec // G304: test-owned invocation log in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(calls), " cat-file "); n != 1 {
		t.Fatalf("ReadCommit started %d cat-file processes, want 1:\n%s", n, calls)
	}
}

// A blob-mode entry must name a blob. `cat-file blob` used to peel a tag to
// the blob it points at; the batch read refuses it instead.
func TestReadCommitRefusesATagInABlobEntry(t *testing.T) {
	repo := newRepo(t)
	blob := git(t, repo, "rev-parse", "HEAD:policy/README.md")
	git(t, repo, "tag", "-a", "blob-tag", "-m", "tag", blob)
	tag := git(t, repo, "rev-parse", "blob-tag")
	raw, err := hex.DecodeString(tag)
	if err != nil {
		t.Fatal(err)
	}
	// mktree refuses a type mismatch, so the tree is written byte for byte.
	agents := gitWithInput(t, repo, append([]byte("100644 linked\x00"), raw...),
		"hash-object", "-t", "tree", "-w", "--stdin")
	policy := gitWithInput(t, repo, []byte("040000 tree "+agents+"\tagents\n"), "mktree")
	root := gitWithInput(t, repo, []byte("040000 tree "+policy+"\tpolicy\n"), "mktree")
	commit := git(t, repo, "commit-tree", root, "-m", "tag in a blob entry")

	if _, err := agenttree.ReadCommit(t.Context(), repo, t.TempDir(), commit); !errors.Is(err, agenttree.ErrMalformed) {
		t.Fatalf("ReadCommit = %v, want %v", err, agenttree.ErrMalformed)
	}
}
