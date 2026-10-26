package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// closureCauseForge is the fake forge's answers: whether the repository
// visibility and the source issue read succeed.
type closureCauseForge struct {
	visibility, issue bool
}

func (f closureCauseForge) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/repos/" + closureStoreRepo:
		if f.visibility {
			_, _ = w.Write([]byte(`{"visibility":"public","private":false}`))
			return
		}
	case "/repos/" + closureStoreRepo + "/issues/7":
		if f.issue {
			_, _ = w.Write([]byte(`{"number":7,"state":"open","title":"Fix the bug","body":"It crashes."}`))
			return
		}
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

// closureCauseRepo returns a checkout with a base commit and one head. A
// non-empty template is committed on the base as its pull request template.
func closureCauseRepo(t *testing.T, template string) (repo, baseSHA, headSHA string) {
	t.Helper()
	repo = t.TempDir()
	runRemediationGit(t, repo, nil, "init", "-q", "-b", "main", "--object-format=sha1")
	commit := func(name, body, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		runRemediationGit(t, repo, nil, "add", "-A")
		runRemediationGit(t, repo, nil, "commit", "-q", "-m", message)
		return strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))
	}
	if template != "" {
		commit("pull_request_template.md", template, "template")
	}
	return repo, commit("file.txt", "one\n", "base"), commit("file.txt", "one\ntwo\n", "head")
}

// closureCauseArtifacts is an artifact store whose every blob is unreadable.
type closureCauseArtifacts struct{ ArtifactStore }

func (closureCauseArtifacts) Open(domain.Digest) (io.ReadCloser, error) {
	return nil, errors.New("blob unavailable")
}

func closureCauseClient(t *testing.T, driver inference.Driver, sites ...inference.Site) *inference.Client {
	t.Helper()
	root := t.TempDir()
	claims, err := advisory.Open(filepath.Join(root, "claims.json"), 100, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(root, "ledger.json"), Advisory: claims,
		Roles: inferencefake.Roles{Provider: "fake", Model: "closure", Driver: driver}, Sites: sites,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func closureCauseBudget() inference.Budget {
	limits := inference.Limits{Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour}
	return inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
	}
}

// TestProposeClosureRecordsFallbackCause proves every closure fallback records
// its fixed cause in the checkpoint, end to end through the real author-input
// builder, inference client, and admission: the cause names the failed step or
// fallback code and never copies the underlying error.
func TestProposeClosureRecordsFallbackCause(t *testing.T) {
	t.Parallel()
	oversized := strings.Repeat("x", 64<<10+1)
	for _, tc := range []struct {
		label    string
		forge    closureCauseForge
		template string
		// noSite composes the client without the propose site; noClient
		// leaves the workflow with no inference client at all.
		noSite, noClient bool
		script           *inferencefake.Script
		noProject        bool
		// unknownHead publishes a head the checkout lacks; instructions
		// names an instruction snapshot the artifact store can't open.
		unknownHead, instructions bool
		origin                    domain.ClosureFlagOrigin
		reason                    string
	}{
		{
			label: "no inference client", noClient: true,
			origin: domain.ClosureFlagOriginDaemonFallback, reason: "propose site unavailable",
		},
		{
			label: "propose site not composed", noSite: true,
			origin: domain.ClosureFlagOriginDaemonFallback, reason: "propose site unavailable",
		},
		{
			label:  "visibility read fails",
			origin: domain.ClosureFlagOriginDaemonFallback, reason: "resolve author input failed: read target visibility",
		},
		{
			label: "instruction snapshot unreadable", forge: closureCauseForge{visibility: true}, instructions: true,
			origin: domain.ClosureFlagOriginDaemonFallback, reason: "resolve author input failed: read instruction snapshot",
		},
		{
			label: "candidate diff fails", forge: closureCauseForge{visibility: true}, unknownHead: true,
			origin: domain.ClosureFlagOriginDaemonFallback, reason: "resolve author input failed: render candidate diff",
		},
		{
			// The Wave 8 fixture pass: a source issue the publisher cannot read.
			label: "source issue text read fails", forge: closureCauseForge{visibility: true},
			origin: domain.ClosureFlagOriginDaemonFallback, reason: "resolve author input failed: read source issue text",
		},
		{
			label: "control file refused", forge: closureCauseForge{visibility: true, issue: true}, template: oversized,
			origin: domain.ClosureFlagOriginDaemonFallback,
			reason: "author input refused: " + (&inference.AuthorControlFileError{
				Field: "pr_template", Bytes: len(oversized), Limit: 64 << 10, Cause: "exceeds author input budget",
			}).Error(),
		},
		{
			// An unscoped call is the client's error with no fallback result.
			label: "call fails", forge: closureCauseForge{visibility: true, issue: true}, noProject: true,
			origin: domain.ClosureFlagOriginDaemonFallback, reason: "propose call failed",
		},
		{
			label: "call times out", forge: closureCauseForge{visibility: true, issue: true},
			script: &inferencefake.Script{Err: context.DeadlineExceeded},
			origin: domain.ClosureFlagOriginDaemonFallback, reason: "propose site fell back: deadline_exceeded",
		},
		{
			label: "clean answer", forge: closureCauseForge{visibility: true, issue: true},
			script: &inferencefake.Script{Response: inference.Response{Output: []byte(`{"resolves":true}`), ComputeUnits: 1}},
			origin: domain.ClosureFlagOriginProposeSite, reason: "",
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			server := httptest.NewServer(http.HandlerFunc(tc.forge.serve))
			defer server.Close()
			s, policy, taskID := seedClosureChain(t, "run-closure-cause")
			repo, baseSHA, headSHA := closureCauseRepo(t, tc.template)
			driver := inferencefake.New()
			if tc.script != nil {
				driver.Script(inference.PublicationAuthorProposeSiteID, *tc.script)
			}
			sites := []inference.Site{inference.PublicationAuthorProposeSite(closureCauseBudget())}
			if tc.noSite {
				sites = []inference.Site{inference.TaskNamerSite(closureCauseBudget())}
			}
			w := &productionPublicationWorkflow{
				store: s, workDir: t.TempDir(),
				publisher:     publish.NewPublisher(sourceIssueTestToken{}, server.Client(), server.URL, nil, nil, nil, nil),
				claimDispatch: func(context.Context, domain.RunID) error { return nil },
			}
			if !tc.noClient {
				w.inference = closureCauseClient(t, driver, sites...)
			}
			binding := closureStoreBinding(policy, taskID)
			binding.admission.Base.BaseSHA = baseSHA
			task := productionPublicationTask{
				RunID: policy.RunID, ProjectID: closureStoreProject, PublicationID: "pub-1", HeadSHA: headSHA,
				Publication: ProductionPublication{Recipe: clientPublicationRecipeV2},
			}
			if tc.noProject {
				task.ProjectID = ""
			}
			if tc.unknownHead {
				task.HeadSHA = strings.Repeat("0", 40)
			}
			var instructions exec.ReviewInstructionBinding
			if tc.instructions {
				w.artifacts = closureCauseArtifacts{}
				instructions.ResultDigest = domain.Digest("sha256:" + strings.Repeat("f", 64))
			}
			decided, err := w.reconcileClosureDecision(
				ctx, task, binding, productionVerificationCheckpoint{}, instructions, repo, nil)
			if err != nil {
				t.Fatalf("reconcile closure decision: %v", err)
			}
			want := productionClosureCheckpoint{
				Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: task.PublicationID,
			}
			stored, found, err := w.loadClosureCheckpoint(ctx, productionClosureCheckpointKey(task.RunID, task.PublicationID), want)
			if err != nil || !found {
				t.Fatalf("load closure checkpoint: found=%t err=%v", found, err)
			}
			if !stored.HasProposal || stored.Origin != tc.origin || stored.Reason != tc.reason {
				t.Fatalf("stored checkpoint = %+v, want origin %q reason %q", stored, tc.origin, tc.reason)
			}
			if stored != decided {
				t.Fatalf("stored checkpoint %+v differs from the decided %+v", stored, decided)
			}
		})
	}
}

// TestProposeClosureRetriesSourceIssueCheckpointFault proves a store fault on
// the shared source-issue checkpoint propagates for retry, as it does in
// authoring, instead of fixing a permanent daemon_fallback.
func TestProposeClosureRetriesSourceIssueCheckpointFault(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	server := httptest.NewServer(http.HandlerFunc(closureCauseForge{visibility: true, issue: true}.serve))
	defer server.Close()
	s, policy, taskID := seedClosureChain(t, "run-closure-retry")
	repo, baseSHA, headSHA := closureCauseRepo(t, "")
	w := &productionPublicationWorkflow{
		store: s, workDir: t.TempDir(),
		publisher:     publish.NewPublisher(sourceIssueTestToken{}, server.Client(), server.URL, nil, nil, nil, nil),
		inference:     closureCauseClient(t, inferencefake.New(), inference.PublicationAuthorProposeSite(closureCauseBudget())),
		claimDispatch: func(context.Context, domain.RunID) error { return nil },
	}
	binding := closureStoreBinding(policy, taskID)
	binding.admission.Base.BaseSHA = baseSHA
	task := productionPublicationTask{
		RunID: policy.RunID, ProjectID: closureStoreProject, PublicationID: "pub-1", HeadSHA: headSHA,
		Publication: ProductionPublication{Recipe: clientPublicationRecipeV2},
	}
	// A row of the wrong kind under the source-issue checkpoint's key.
	sourceKey := "production-source-issue/" + string(task.RunID) + "/" + headSHA + "/" + baseSHA
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		_, _, err := tx.RecordInbox(ctx, sourceKey, "not_a_source_issue_checkpoint", []byte(`{}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := w.reconcileClosureDecision(
		ctx, task, binding, productionVerificationCheckpoint{}, exec.ReviewInstructionBinding{}, repo, nil)
	if !errors.Is(err, errSourceIssueCheckpoint) {
		t.Fatalf("reconcile closure decision error = %v, want the source-issue checkpoint fault", err)
	}
	want := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: task.PublicationID,
	}
	if _, found, err := w.loadClosureCheckpoint(ctx, productionClosureCheckpointKey(task.RunID, task.PublicationID), want); err != nil || found {
		t.Fatalf("closure checkpoint after a store fault: found=%t err=%v, want none", found, err)
	}
}

// TestClosureCauseBackstops pins the two causes the builder and the inference
// client never produce today: an untagged input failure and a fallback that
// names neither a refusal nor a code.
func TestClosureCauseBackstops(t *testing.T) {
	t.Parallel()
	if got := closureInputFailureCause(errors.New("untagged")); got != "resolve author input failed" {
		t.Errorf("untagged input failure cause = %q", got)
	}
	if got := closureCallFallbackCause(inference.ProposedClosure{Fallback: true}); got != "author input refused" {
		t.Errorf("bare fallback cause = %q", got)
	}
}
