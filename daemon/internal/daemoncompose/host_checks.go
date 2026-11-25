package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// maxSeedEntries bounds the seed-root walk, as the preflight's seed_root
// check does.
const maxSeedEntries = 100_000

// HostCheckError is a named start check that failed: the check, what it
// found, and the operator's fix. The installed daemon serves driverless
// with a card that names all three (ExecutionHostCheckFailed); any other
// caller reads it as an ordinary error. Check carries the preflight's name
// for the same check where one exists, so a card and a preflight manifest
// name a failure the same way.
type HostCheckError struct {
	Check, Evidence, Fix string
	Err                  error
}

func (e *HostCheckError) Error() string {
	return fmt.Sprintf("%s: %s; %s", e.Check, e.Evidence, e.Fix)
}

func (e *HostCheckError) Unwrap() error { return e.Err }

// hostProbes is what the host checks reach outside the process.
type hostProbes struct {
	imageInStore func(ctx context.Context, containerBin, ref string) error
	lookPath     func(file string) (string, error)
	seedClean    func(ctx context.Context, dir string) error
}

func productionHostProbes() hostProbes {
	return hostProbes{
		imageInStore: CheckImageInStore,
		lookPath:     osexec.LookPath,
		seedClean: func(ctx context.Context, dir string) error {
			return ward.CheckCanonicalSeedWorkspaceClean(ctx, dir, maxSeedEntries)
		},
	}
}

// checkHost runs the host half of the preflight that needs no rig, before
// the installed daemon composes its driver, and returns the first failure as
// a HostCheckError. These are the checks whose failure composition would
// otherwise report late or without a fix. What else the preflight checks is
// covered elsewhere: the settings loader refuses a malformed section, the
// readiness record holds the per-project checks, composition's own steps
// fail by name through namedCompositionFailure, and the rig's checks have no
// rig to check.
func checkHost(ctx context.Context, cfg ClaudeDriverConfig, probes hostProbes) error {
	if info, err := os.Stat(cfg.ContainerBin); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return &HostCheckError{
			Check:    "host_tools.container",
			Evidence: "no executable container CLI is at " + cfg.ContainerBin,
			Fix: "install the Apple container CLI there, or set its path with freesided settings set " +
				"-section host_tools",
			Err: err,
		}
	}
	// launchd's default PATH holds /usr/bin, where macOS keeps git once the
	// command line tools are installed.
	if _, err := probes.lookPath("git"); err != nil {
		return &HostCheckError{
			Check: "host_tools.git", Evidence: "git does not resolve on this daemon's PATH",
			Fix: "install the Xcode command line tools with xcode-select --install", Err: err,
		}
	}
	// The writer's image is the project's, which the readiness record checks.
	images := []struct{ check, ref string }{
		{"exporter_image", cfg.ExporterImage},
		{"reviewer_image", cfg.ReviewImage},
	}
	if cfg.ShadowReviewImage != "" {
		images = append(images, struct{ check, ref string }{"shadow_reviewer_image", cfg.ShadowReviewImage})
	}
	for _, image := range images {
		if err := probes.imageInStore(ctx, cfg.ContainerBin, image.ref); err != nil {
			evidence, fix := ImageStoreFailure(image.ref, err)
			return &HostCheckError{Check: image.check, Evidence: evidence, Fix: fix, Err: err}
		}
	}
	if err := probes.seedClean(ctx, cfg.SeedRoot); err != nil {
		return &HostCheckError{
			Check: "seed_root", Evidence: "seed root " + cfg.SeedRoot + " is dirty or unreadable",
			Fix: "restore the exact-base clean seed checkout, or set another with freesided settings set " +
				"-section seed_root",
			Err: err,
		}
	}
	if _, err := ReadPromptPackages(cfg); err != nil {
		return &HostCheckError{
			Check: "prompt_packages", Evidence: err.Error(),
			Fix: "correct the prompt package files, or set others with freesided settings set -section prompts",
			Err: err,
		}
	}
	forbidden := []string{cfg.ReviewAuthSnapshot}
	if cfg.ShadowReviewAuthSnapshot != "" {
		forbidden = append(forbidden, cfg.ShadowReviewAuthSnapshot)
	}
	if _, err := engine.SnapshotReviewHostInstructions(ctx, cfg.ReviewInstructions, forbidden...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &HostCheckError{
			Check: "review_instructions", Evidence: "review host instructions are absent, unreadable, or invalid",
			Fix: "provide a stable review-instructions file outside the credential snapshot, set with " +
				"freesided settings set -section review",
			Err: err,
		}
	}
	return nil
}

// namedCompositionFailure names a composition failure whose cause the
// operator fixes on the host or in the settings, or returns nil for one it
// cannot name, which keeps the composition_failed card and its error text.
// It names the step's own HostCheckError (the macOS verification executor's
// conformance pass) and the agent selection that resolves no line.
func namedCompositionFailure(err error) *HostCheckError {
	var (
		named            *HostCheckError
		roleFailure      *RoleAdmissionError
		providerMismatch *ReviewProviderMismatch
	)
	switch {
	case errors.As(err, &named):
		return named
	case errors.As(err, &providerMismatch):
		return &HostCheckError{
			Check: "agent_selection", Evidence: providerMismatch.Error(),
			Fix: "select a reviewer agent whose auth mode the review section accepts, then adopt it with " +
				"freesided auth adopt",
			Err: err,
		}
	case errors.As(err, &roleFailure):
		return &HostCheckError{
			Check:    "agent_selection",
			Evidence: fmt.Sprintf("role %s has no lineup line that resolves against the store", roleFailure.Role),
			Fix: "adopt the agent tree at the commit the agent_tree section names with freesided auth adopt, " +
				"or set another commit with freesided settings set -section agent_tree",
			Err: err,
		}
	}
	return nil
}
