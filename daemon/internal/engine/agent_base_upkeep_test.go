package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	upkeepDebian = "docker.io/library/debian:trixie-slim@sha256:" +
		"020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd"
	upkeepHarnessBuild = "claude-code 2.1.319"
)

var upkeepBaseRef = domain.ImageRef("ghcr.io/owner/freeside-agent-claude@sha256:" + strings.Repeat("e", 64))

func upkeepContainerfile(version, comment string) string {
	return "# " + comment + "\nARG BASE_IMAGE=" + upkeepDebian + "\nARG CLAUDE_CODE_VERSION=" + version +
		"\nFROM ${BASE_IMAGE}\n"
}

// agentBaseBuilder builds a base by tagging it in tags, as the real builder
// leaves AgentBaseLocalTag on the host.
type agentBaseBuilder struct {
	requests []projectimage.AgentBaseRequest
	tags     map[string]string
	fail     error
}

func (b *agentBaseBuilder) BuildAgentBase(
	_ context.Context, request projectimage.AgentBaseRequest,
) (projectimage.AgentBase, error) {
	b.requests = append(b.requests, request)
	if b.fail != nil {
		return projectimage.AgentBase{}, b.fail
	}
	tag := projectimage.AgentBaseLocalTag(upkeepBaseRef)
	_, digest, _ := strings.Cut(string(upkeepBaseRef), "@")
	b.tags[tag] = digest
	return projectimage.AgentBase{ImageRef: upkeepBaseRef, ClaudeVersion: request.ClaudeVersion, LocalTag: tag}, nil
}

// agentBaseHarness is an upkeep harness whose repository carries the agent
// base source at the tip and whose pointed-to image is current but for its
// base, and an agent_base setting naming that repository.
func agentBaseHarness(t *testing.T) (*upkeepHarness, *agentBaseBuilder, string) {
	t.Helper()
	repo := newUpkeepRepo(t)
	repo.npmBase()
	tip := repo.commit(map[string]string{
		domain.AgentBaseSourceDir + "/Containerfile": upkeepContainerfile("2.1.319", "one"),
	})
	h := newUpkeepHarness(t, openUpkeepStore(t), repo, "")
	h.seed(upkeepImage(t, tip, func(in *domain.ProjectImageInput) {
		in.Environment.PreparationDigest = projectimage.PreparationDigest()
	}))
	h.observeTip(tip)
	builder := &agentBaseBuilder{tags: map[string]string{}}
	h.upkeep.AgentBase = &AgentBaseUpkeep{
		Builder: builder,
		Source: func(context.Context) (*domain.AgentBaseSettings, error) {
			return &domain.AgentBaseSettings{RepositoryID: 42, Repository: "owner/repo"}, nil
		},
		Repository: "owner/repo", HarnessBuild: upkeepHarnessBuild,
		LocalDigest: func(_ context.Context, ref string) (string, error) {
			digest, ok := builder.tags[ref]
			if !ok {
				return "", errors.New("no such image")
			}
			return digest, nil
		},
	}
	return h, builder, tip
}

func (h *upkeepHarness) agentBase() (domain.AdmittedAgentBase, bool) {
	h.t.Helper()
	var (
		base  domain.AdmittedAgentBase
		found bool
	)
	if err := h.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		_, base, found, err = tx.GetAdmittedAgentBasePointer(context.Background(), domain.HarnessClientClaudeCode)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return base, found
}

// A source the daemon pins builds a base beside the project image, which
// the pass admits and then rebuilds the project image FROM through the
// base's local tag. A later pass builds neither again; a pass that finds
// the local tag gone builds the base again and leaves the image alone.
func TestAgentBaseUpkeepRebuildsTheImageOntoANewBase(t *testing.T) {
	t.Parallel()
	h, bases, tip := agentBaseHarness(t)
	seeded := h.pointer()

	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(bases.requests) != 1 {
		t.Fatalf("base builds = %+v, want one", bases.requests)
	}
	request := bases.requests[0]
	if request.CommitSHA != tip || request.ClaudeVersion != "2.1.319" || request.Destination != seeded.ImageRef {
		t.Fatalf("base build = %+v, want the tip's pin published beside %s", request, seeded.ImageRef)
	}
	base, found := h.agentBase()
	if !found || base.ImageRef != upkeepBaseRef || base.HarnessBuild != upkeepHarnessBuild ||
		base.SourceCommit != tip || base.SourceRepository != "owner/repo" {
		t.Fatalf("admitted base = %+v found=%v", base, found)
	}
	if len(h.builder.requests) != 1 {
		t.Fatalf("project builds = %d, want one onto the new base", len(h.builder.requests))
	}
	if got := h.builder.requests[0]; got.BaseImageRef != upkeepBaseRef ||
		got.BaseBuildRef != projectimage.AgentBaseLocalTag(upkeepBaseRef) {
		t.Fatalf("project build base %s through %s, want %s through its local tag",
			got.BaseImageRef, got.BaseBuildRef, upkeepBaseRef)
	}
	if image := h.pointer(); image.BaseImageRef != upkeepBaseRef {
		t.Fatalf("pointer names an image on %s, want %s", image.BaseImageRef, upkeepBaseRef)
	}
	if len(h.faults) != 1 || h.faults[0] != "" {
		t.Fatalf("health = %q, want a cleared item", h.faults)
	}
	if got := h.upkeep.TipAgentBaseVersion(); got != "2.1.319" {
		t.Fatalf("tip version = %q", got)
	}

	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(bases.requests) != 1 || len(h.builder.requests) != 1 {
		t.Fatal("a pass over a current base and image built again")
	}

	clear(bases.tags)
	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(bases.requests) != 2 || len(h.builder.requests) != 1 {
		t.Fatalf("after the tag went: base builds %d, project builds %d; want the base rebuilt and the image kept",
			len(bases.requests), len(h.builder.requests))
	}

	// A change the build does not read moves the tree but rebuilds the same
	// image: it is admitted again from the new tree, and the next pass
	// settles on it.
	readme := h.repo.commit(map[string]string{domain.AgentBaseSourceDir + "/README.md": "notes\n"})
	h.observeTip(readme)
	for range 2 {
		if err := h.upkeep.RunAll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(bases.requests) != 3 || len(h.builder.requests) != 1 {
		t.Fatalf("after a README change: base builds %d, project builds %d; want one base rebuild and the image kept",
			len(bases.requests), len(h.builder.requests))
	}
	if base, found := h.agentBase(); !found || base.SourceCommit != readme || base.ImageRef != upkeepBaseRef {
		t.Fatalf("admitted base = %+v found=%v, want %s admitted again at %s", base, found, upkeepBaseRef, readme)
	}
}

// agentBaseBarrier holds each base build until release closes, after
// telling arrived it started.
type agentBaseBarrier struct {
	inner   *agentBaseBuilder
	arrived chan struct{}
	release chan struct{}
}

func (b *agentBaseBarrier) BuildAgentBase(
	ctx context.Context, request projectimage.AgentBaseRequest,
) (projectimage.AgentBase, error) {
	b.arrived <- struct{}{}
	<-b.release
	return b.inner.BuildAgentBase(ctx, request)
}

// At a build limit of two, a second pass for the same repository waits for
// the first's base build, then finds the base the first admitted and builds
// neither the base nor the image again.
func TestAgentBaseUpkeepPassesTakeTurnsAtAnyLimit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, bases, _ := agentBaseHarness(t)
		barrier := &agentBaseBarrier{inner: bases, arrived: make(chan struct{}, 2), release: make(chan struct{})}
		h.upkeep.AgentBase.Builder = barrier
		h.upkeep.Rebuild.BuildLimit = 2
		done := make(chan error, 2)
		go func() { done <- h.upkeep.RunAll(t.Context()) }()
		<-barrier.arrived
		go func() { done <- h.upkeep.RunScheduled(t.Context()) }()
		synctest.Wait()
		select {
		case <-barrier.arrived:
			t.Fatal("a second pass for one repository built the agent base beside the first")
		default:
		}
		close(barrier.release)
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		if len(bases.requests) != 1 || len(h.builder.requests) != 1 {
			t.Fatalf("base builds %d, project builds %d; want the first pass's alone",
				len(bases.requests), len(h.builder.requests))
		}
		if image := h.pointer(); image.BaseImageRef != upkeepBaseRef {
			t.Fatalf("pointer names an image on %s, want %s", image.BaseImageRef, upkeepBaseRef)
		}
	})
}

// Upkeep builds a base only when the setting names the repository it serves
// and the tip pins the CLI the daemon pins. Otherwise it builds nothing,
// keeps the image, and says why.
func TestAgentBaseUpkeepBuildsNothingItMayNotBuild(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit  func(*upkeepHarness)
		fault string
	}{
		"setting unset": {edit: func(h *upkeepHarness) {
			h.upkeep.AgentBase.Source = func(context.Context) (*domain.AgentBaseSettings, error) { return nil, nil }
		}},
		"setting refused": {edit: func(h *upkeepHarness) {
			h.upkeep.AgentBase.Source = func(context.Context) (*domain.AgentBaseSettings, error) {
				return nil, errors.New("refused by current policy")
			}
		}, fault: "setting could not be read"},
		"setting names another repository": {edit: func(h *upkeepHarness) {
			h.upkeep.AgentBase.Source = func(context.Context) (*domain.AgentBaseSettings, error) {
				return &domain.AgentBaseSettings{RepositoryID: 7, Repository: "owner/fork"}, nil
			}
		}, fault: "not the repository this daemon serves"},
		"tip pins another release": {edit: func(h *upkeepHarness) {
			h.upkeep.AgentBase.HarnessBuild = "claude-code 2.1.318"
		}, fault: "waits for a daemon update"},
		"tip source unreadable": {edit: func(h *upkeepHarness) {
			tip := h.repo.commit(map[string]string{
				domain.AgentBaseSourceDir + "/Containerfile": "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM debian\n",
			})
			h.observeTip(tip)
		}, fault: "could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, bases, _ := agentBaseHarness(t)
			tc.edit(h)
			seeded := h.pointer()
			if err := h.upkeep.RunAll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(bases.requests) != 0 {
				t.Fatalf("base builds = %+v, want none", bases.requests)
			}
			if _, found := h.agentBase(); found {
				t.Fatal("a base was admitted")
			}
			if h.pointer().BaseImageRef != seeded.BaseImageRef {
				t.Fatal("the image moved off its base")
			}
			if len(h.faults) != 1 || !strings.Contains(h.faults[0], tc.fault) || (tc.fault == "") != (h.faults[0] == "") {
				t.Fatalf("health = %q, want a fault containing %q", h.faults, tc.fault)
			}
		})
	}
}

// The admitted pointer applies only while the setting names the repository
// the base was built from: once the operator clears or retargets it, an
// image stays on the base it has rather than moving onto the stored base.
func TestAgentBaseUpkeepAppliesABaseOnlyWhileTheSettingNamesIt(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]func(context.Context) (*domain.AgentBaseSettings, error){
		"setting cleared": func(context.Context) (*domain.AgentBaseSettings, error) { return nil, nil },
		"setting refused": func(context.Context) (*domain.AgentBaseSettings, error) {
			return nil, errors.New("refused by current policy")
		},
		"setting names another repository": func(context.Context) (*domain.AgentBaseSettings, error) {
			return &domain.AgentBaseSettings{RepositoryID: 7, Repository: "owner/fork"}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, bases, tip := agentBaseHarness(t)
			stored, err := domain.NewAdmittedAgentBase(domain.AdmittedAgentBaseInput{
				Harness: imageUpkeepHarness, ImageRef: upkeepBaseRef, HarnessBuild: upkeepHarnessBuild,
				SourceRepoID: 42, SourceRepository: "owner/repo", SourceCommit: tip,
				SourceTreeSHA: strings.Repeat("2", 40), AdmittedAt: h.upkeep.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
				recorded, err := tx.RecordAdmittedAgentBase(t.Context(), stored)
				if err != nil {
					return err
				}
				return tx.AdvanceAdmittedAgentBasePointer(t.Context(), domain.AdmittedAgentBasePointer{
					Harness: imageUpkeepHarness, BaseID: recorded.ID, AdvancedAt: recorded.AdmittedAt,
				})
			}); err != nil {
				t.Fatal(err)
			}
			h.upkeep.AgentBase.Source = source
			seeded := h.pointer()
			if err := h.upkeep.RunAll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(bases.requests) != 0 || len(h.builder.requests) != 0 {
				t.Fatalf("base builds %d, project builds %d; want none", len(bases.requests), len(h.builder.requests))
			}
			if h.pointer().BaseImageRef != seeded.BaseImageRef {
				t.Fatalf("the image moved onto %s, want it kept on %s", h.pointer().BaseImageRef, seeded.BaseImageRef)
			}
		})
	}
}

// A host fault, unlike a source the build refuses, is not remembered: the
// next pass builds the same tree and pin again.
func TestAgentBaseUpkeepRetriesAHostFault(t *testing.T) {
	t.Parallel()
	h, bases, _ := agentBaseHarness(t)
	bases.fail = errors.New("the image builder did not start")
	for range 2 {
		if err := h.upkeep.RunAll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(bases.requests) != 2 {
		t.Fatalf("base builds = %d, want one per pass", len(bases.requests))
	}
	if len(h.faults) != 2 || !strings.Contains(h.faults[1], "did not start") {
		t.Fatalf("health = %q, want the host fault on each pass", h.faults)
	}
}

// A stored base this daemon's pin doesn't carry, as after the operator
// installs an older daemon, does not apply: the image stays on the base it
// has while the tip's pin waits for a matching daemon.
func TestAgentBaseUpkeepAppliesABaseOnlyAtTheDaemonsPin(t *testing.T) {
	t.Parallel()
	h, bases, _ := agentBaseHarness(t)
	tip := h.repo.commit(map[string]string{
		domain.AgentBaseSourceDir + "/Containerfile": upkeepContainerfile("2.1.320", "newer"),
	})
	h.observeTip(tip)
	stored, err := domain.NewAdmittedAgentBase(domain.AdmittedAgentBaseInput{
		Harness: imageUpkeepHarness, ImageRef: upkeepBaseRef, HarnessBuild: "claude-code 2.1.320",
		SourceRepoID: 42, SourceRepository: "owner/repo", SourceCommit: tip,
		SourceTreeSHA: strings.Repeat("3", 40), AdmittedAt: h.upkeep.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		recorded, err := tx.RecordAdmittedAgentBase(t.Context(), stored)
		if err != nil {
			return err
		}
		return tx.AdvanceAdmittedAgentBasePointer(t.Context(), domain.AdmittedAgentBasePointer{
			Harness: imageUpkeepHarness, BaseID: recorded.ID, AdvancedAt: recorded.AdmittedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	seeded := h.pointer()
	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(bases.requests) != 0 || len(h.builder.requests) != 0 {
		t.Fatalf("base builds %d, project builds %d; want none", len(bases.requests), len(h.builder.requests))
	}
	if h.pointer().BaseImageRef != seeded.BaseImageRef {
		t.Fatalf("the image moved onto %s, want it kept on %s", h.pointer().BaseImageRef, seeded.BaseImageRef)
	}
	if len(h.faults) != 1 || !strings.Contains(h.faults[0], "waits for a daemon update") {
		t.Fatalf("health = %q, want the pin's fault", h.faults)
	}
}

// A base that failed to build is not built again for the same source and
// pin; the image stays on its base and the fault stays raised. A change to
// the source builds again.
func TestAgentBaseUpkeepRemembersAFailedBuild(t *testing.T) {
	t.Parallel()
	h, bases, _ := agentBaseHarness(t)
	bases.fail = fmt.Errorf("probe refused the image: %w", projectimage.ErrProofFailed)
	seeded := h.pointer()
	for range 2 {
		if err := h.upkeep.RunAll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(bases.requests) != 1 {
		t.Fatalf("base builds = %d, want one for one source and pin", len(bases.requests))
	}
	if len(h.faults) != 2 || h.faults[0] != h.faults[1] ||
		!strings.Contains(h.faults[0], "could not be built") || !strings.Contains(h.faults[0], "probe refused") {
		t.Fatalf("health = %q, want the build's fault on each pass", h.faults)
	}
	if h.pointer().ID != seeded.ID {
		t.Fatal("a failed base build moved the image")
	}

	bases.fail = nil
	tip := h.repo.commit(map[string]string{
		domain.AgentBaseSourceDir + "/Containerfile": upkeepContainerfile("2.1.319", "two"),
	})
	h.observeTip(tip)
	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(bases.requests) != 2 || bases.requests[1].CommitSHA != tip {
		t.Fatalf("base builds = %+v, want a second at the changed source", bases.requests)
	}
	if base, found := h.agentBase(); !found || base.SourceCommit != tip {
		t.Fatalf("admitted base = %+v found=%v, want the one built at %s", base, found, tip)
	}
}
