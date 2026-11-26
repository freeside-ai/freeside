package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// AgentBaseBuilder builds, probes, and publishes an agent base from a
// checkout (projectimage.Builder.BuildAgentBase).
type AgentBaseBuilder interface {
	BuildAgentBase(context.Context, projectimage.AgentBaseRequest) (projectimage.AgentBase, error)
}

// AgentBaseUpkeep is what image upkeep needs to keep the Claude agent base
// current (plan §5.7, Agent-base upkeep). Upkeep builds a base only from the
// repository it serves, only from a source whose CLI pin is the one this
// daemon's adapter pins, and only when the operator's agent_base setting
// names that repository. Review at merge is the gate on the source; the
// build's probes are the gate on the image.
type AgentBaseUpkeep struct {
	Builder AgentBaseBuilder
	// Source reads the agent_base instance setting at the pass, nil when
	// unset. It is read on every pass, so a change takes effect at the next.
	Source func(context.Context) (*domain.AgentBaseSettings, error)
	// Repository is the slug of the repository the daemon serves, which a
	// source setting must name together with its ID.
	Repository string
	// HarnessBuild is the harness build this daemon's adapter pins
	// (agentbaseline.ClaudeCodeHarnessBuild).
	HarnessBuild string
	// LocalDigest resolves a local image reference to its digest, so a pass
	// can tell whether an admitted base's local tag still names it.
	LocalDigest func(ctx context.Context, ref string) (string, error)
}

// agentBaseFailure is a base build that failed for a source tree and pin.
// A pass does not build that pair again until the tip's source or the
// daemon's pin changes, or the daemon restarts, so a failing build does not
// repeat on every scheduled pass.
type agentBaseFailure struct {
	tree, harnessBuild, fault string
}

// harnessBuildFor is the harness build a source's CLI pin names, in the form
// the adapter pins it.
func harnessBuildFor(claudeVersion string) string {
	return "claude-code " + claudeVersion
}

// keepAgentBase brings the admitted agent base up to the tip's source, when
// agent-base upkeep is configured, and returns the base the pointed-to
// project image should build FROM: the admitted base, or nil when the daemon
// has admitted none. A fault says why the base could not be kept current;
// the project image is still kept on the base the daemon last admitted.
//
// The stored pointer decides every project image's base, so it is re-gated
// against the current setting and this daemon's pin on each pass: it
// applies only while the agent_base setting names the repository this
// daemon serves, the base was built from it, and the base carries the
// harness build this daemon pins. Otherwise nil leaves each image on the
// base it has.
func (u *ImageUpkeep) keepAgentBase(
	ctx context.Context, dir, tipSHA string, image domain.ProjectImage,
) (*domain.AdmittedAgentBase, string, error) {
	keeper := u.AgentBase
	if keeper == nil {
		return nil, "", nil
	}
	settings, err := keeper.Source(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", err
		}
		return nil, "the agent_base setting could not be read: " + boundedRebuildText(err.Error()), nil
	}
	if settings == nil {
		return nil, "", nil
	}
	if settings.RepositoryID != u.RepositoryID || settings.Repository != keeper.Repository {
		return nil, fmt.Sprintf(
			"the agent_base setting names %s (id %d), not the repository this daemon serves, %s (id %d)",
			settings.Repository, settings.RepositoryID, keeper.Repository, u.RepositoryID), nil
	}
	// Taken before the pointer read and held through the admit, so this
	// repository's other pass waits, then reads the base this one admitted
	// instead of building it again. It is released before rebuild takes it.
	if err := u.pointerTurn.acquire(ctx, 1); err != nil {
		return nil, "", err
	}
	defer u.pointerTurn.release()
	var (
		base  domain.AdmittedAgentBase
		found bool
	)
	if err := u.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		_, base, found, err = tx.GetAdmittedAgentBasePointer(ctx, imageUpkeepHarness)
		return err
	}); err != nil {
		return nil, "", fmt.Errorf("read admitted agent base pointer: %w", err)
	}
	var admitted *domain.AdmittedAgentBase
	if found && base.SourceRepoID == settings.RepositoryID && base.SourceRepository == settings.Repository &&
		base.HarnessBuild == keeper.HarnessBuild {
		admitted = &base
	}
	source, err := projectimage.ReadAgentBaseSource(ctx, "git", dir, tipSHA)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", err
		}
		return admitted, "the agent base source at the tip could not be read: " +
			boundedRebuildText(err.Error()), nil
	}
	u.settledMu.Lock()
	u.tipBaseVersion = source.ClaudeVersion
	u.settledMu.Unlock()
	pinned := harnessBuildFor(source.ClaudeVersion)
	if pinned != keeper.HarnessBuild {
		return admitted, fmt.Sprintf(
			"the agent base at the tip pins %s; it waits for a daemon update that pins the same release (this daemon pins %s)",
			pinned, keeper.HarnessBuild), nil
	}
	if admitted != nil && admitted.SourceTreeSHA == source.TreeSHA && admitted.HarnessBuild == pinned {
		usable, err := u.agentBaseTagged(ctx, *admitted)
		if err != nil || usable {
			return admitted, "", err
		}
	}
	u.settledMu.Lock()
	failed := u.baseFailed
	u.settledMu.Unlock()
	if failed != nil && failed.tree == source.TreeSHA && failed.harnessBuild == pinned {
		return admitted, failed.fault, nil
	}
	built, fault, remember, err := u.buildAgentBase(ctx, dir, tipSHA, image, source)
	if err != nil || built == nil {
		if remember {
			u.settledMu.Lock()
			u.baseFailed = &agentBaseFailure{tree: source.TreeSHA, harnessBuild: pinned, fault: fault}
			u.settledMu.Unlock()
		}
		return admitted, fault, err
	}
	record, err := domain.NewAdmittedAgentBase(domain.AdmittedAgentBaseInput{
		Harness: imageUpkeepHarness, ImageRef: built.ImageRef, HarnessBuild: pinned,
		SourceRepoID: settings.RepositoryID, SourceRepository: settings.Repository,
		SourceCommit: tipSHA, SourceTreeSHA: source.TreeSHA, AdmittedAt: u.Now().UTC(),
	})
	if err != nil {
		return nil, "", fmt.Errorf("admit the agent base built at %s: %w", tipSHA, err)
	}
	if err := u.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		stored, err := tx.RecordAdmittedAgentBase(ctx, record)
		if err != nil {
			return err
		}
		record = stored
		return tx.AdvanceAdmittedAgentBasePointer(ctx, domain.AdmittedAgentBasePointer{
			Harness: imageUpkeepHarness, BaseID: stored.ID, AdvancedAt: u.Now().UTC(),
		})
	}); err != nil {
		return nil, "", fmt.Errorf("admit the agent base built at %s: %w", tipSHA, err)
	}
	u.settledMu.Lock()
	u.baseFailed = nil
	u.settledMu.Unlock()
	u.Logger.Info("image upkeep: admitted a new agent base",
		"tip", tipSHA, "image", record.ImageRef, "harness_build", record.HarnessBuild)
	return &record, "", nil
}

// agentBaseTagged reports whether the admitted base's local tag still
// resolves to it, so a project build can use it. A tag that is gone or names
// another image is rebuilt from source rather than trusted.
func (u *ImageUpkeep) agentBaseTagged(ctx context.Context, base domain.AdmittedAgentBase) (bool, error) {
	_, digest, _ := strings.Cut(string(base.ImageRef), "@")
	observed, err := u.AgentBase.LocalDigest(ctx, projectimage.AgentBaseLocalTag(base.ImageRef))
	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}
		u.Logger.Info("image upkeep: the admitted agent base's local tag does not resolve; building it again",
			"image", base.ImageRef, "error", err)
		return false, nil
	}
	return observed == digest, nil
}

// buildAgentBase builds the base under the shared gate and a memory
// reservation, as a project rebuild does. It returns the built base, or with
// a nil base, a fault or nothing: an update drain, an execution pause, or a
// lack of memory holds the build without a fault, and the next pass builds.
// Its bool reports a fault the source itself caused (it isn't buildable, its
// install failed, or a probe refused the image), which the same tree and pin
// would cause again. A host fault or the time bound is not remembered, so
// the next pass builds again, as a project rebuild's host fault does.
func (u *ImageUpkeep) buildAgentBase(
	ctx context.Context, dir, tipSHA string, image domain.ProjectImage, source projectimage.AgentBaseSource,
) (*projectimage.AgentBase, string, bool, error) {
	if u.Rebuild == nil {
		return nil, "start the daemon with -base-build-ref to let it build the agent base", false, nil
	}
	if err := u.Rebuild.gate.acquire(ctx, u.Rebuild.BuildLimit); err != nil {
		return nil, "", false, err
	}
	err := u.Rebuild.RecoverLeftBuilds(ctx)
	u.Rebuild.gate.release()
	if err != nil {
		return nil, "", false, err
	}
	reservation, err := u.Rebuild.reserveBuild(ctx, u.Policy.Keys)
	if err != nil {
		return nil, "", false, err
	}
	var absence error
	defer func() {
		if err := reservation.Release(ctx, absence); err != nil {
			u.Logger.Error("image upkeep: release the agent base build's memory reservation",
				"reservation", reservation.ID(), "error", err)
		}
	}()
	if err := u.Rebuild.gate.acquire(ctx, u.Rebuild.BuildLimit); err != nil {
		return nil, "", false, err
	}
	defer u.Rebuild.gate.release()
	end, err := u.Running.BeginUnlessDraining(ctx, u.Store, domain.RestartBlockerImageBuild)
	if store.NewWorkClosed(err) {
		u.Logger.Info("image upkeep: skipped the agent base build while an update drain or an execution pause is in force",
			"tip", tipSHA)
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	defer end()
	timeout := u.Rebuild.Timeout
	if timeout == 0 {
		timeout = defaultProjectImageRebuildTimeout
	}
	// As a project build's, the bound pauses while the build waits for the
	// builder VM's turn, which is not the build's own work.
	bounded, cancel := context.WithCancelCause(reservation.Context(ctx))
	defer cancel(nil)
	bound := startBuildBound(timeout, cancel)
	defer bound.stop()
	turnBound := projectimage.WithTurnBusy(bounded, bound.pause)
	result, err := u.AgentBase.Builder.BuildAgentBase(turnBound, projectimage.AgentBaseRequest{
		SourceDir: dir, CommitSHA: tipSHA, ClaudeVersion: source.ClaudeVersion,
		Destination: image.ImageRef, DNS: append([]string{}, u.Rebuild.Inputs.DNS...),
		BuildProxy: u.Rebuild.Inputs.BuildProxy,
	})
	absence = containersUnproven(err)
	if err == nil {
		return &result, "", false, nil
	}
	if ctx.Err() != nil {
		return nil, "", false, err
	}
	if errors.Is(err, domain.ErrMachineCapacity) {
		u.Logger.Info("image upkeep: skipped the agent base build until memory frees", "tip", tipSHA, "error", err)
		return nil, "", false, nil
	}
	remember := errors.Is(err, projectimage.ErrAgentBaseSource) || errors.Is(err, projectimage.ErrInstallFailed) ||
		errors.Is(err, projectimage.ErrProofFailed)
	if errors.Is(context.Cause(bounded), errBuildBoundExceeded) {
		err = fmt.Errorf("the build and its probes did not finish within %s: %w", timeout, err)
		remember = false
	}
	return nil, "the agent base could not be built at the tip: " + boundedRebuildText(err.Error()), remember, nil
}

// TipAgentBaseVersion is the Claude CLI release the agent base source pinned
// at the last tip a pass read it at, or empty when no pass has. The bump
// schedule reads it so a bump that merged opens no second task while the
// daemon that pins the release is not yet installed.
func (u *ImageUpkeep) TipAgentBaseVersion() string {
	u.settledMu.Lock()
	defer u.settledMu.Unlock()
	return u.tipBaseVersion
}

// joinUpkeepFaults is the one health fault for a pass: the agent base's and
// the project image's, either of which may be empty.
func joinUpkeepFaults(base, image string) string {
	switch {
	case base == "":
		return image
	case image == "":
		return base
	default:
		return base + "; " + image
	}
}
