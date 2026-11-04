package projectimage

import (
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrComposition marks an unattended composition whose configured agent
// image cannot be bound to a matching immutable project-image record.
// Refusing at startup turns the run-482 misconfiguration (a base the pinned
// image was never built from) into a daemon-start failure instead of a run
// that spends implementation before publication binding rejects it.
var ErrComposition = errors.New("configured agent image has no matching project-image record")

// ResolvePreparation binds the configured agent image (agentImage, with the
// configured repo and repositoryID) to the immutable project_images
// provenance production publication already treats as authority
// (engine.productionPublicationWorkflow.loadBinding), and returns a copy of
// the workspace-hydration argv the launch command must run. image and found
// are the result of looking agentImage up in the store. It refuses when no
// record names the image; when its recorded repository or repository ID
// disagrees with the configured one; or when the decoded preparation command
// is not the fixed image-owned helper the builder and onboarding policy
// admit (PreparationPath). Each refusal wraps ErrComposition and names its
// mismatch.
//
// These are the checks the record alone decides, so they run before anything
// reaches the network. Whether an image may serve the configured base is
// writer selection's question (freesided's claudeImageSelector), which can
// need the base tree; a base no image serves holds attempts instead of
// refusing the start.
//
// The command re-gate is the reconstruction trust boundary (AGENTS.md daemon
// conventions): PreparationCommand is a store field decoded by the caller, so
// a corrupted or tampered row that is otherwise self-consistent must not
// reach the root launch argv. Validate only rejects empty/NUL commands and
// shell-quoting only prevents injection, so neither bounds *which* command
// runs; onboarding gates the same field to the identical fixed value, and this
// boundary re-runs that gate rather than trusting the decoded argv.
func ResolvePreparation(
	image domain.ProjectImage, found bool, agentImage domain.ImageRef, repo string, repositoryID int64,
) ([]string, error) {
	switch {
	case !found:
		return nil, fmt.Errorf("agent image %q: %w", agentImage, ErrComposition)
	case image.Repository != repo:
		return nil, fmt.Errorf(
			"project-image repository %q disagrees with configured %q: %w",
			image.Repository, repo, ErrComposition)
	case image.RepositoryID != repositoryID:
		return nil, fmt.Errorf(
			"project-image repository id %d disagrees with configured %d: %w",
			image.RepositoryID, repositoryID, ErrComposition)
	case !slices.Equal(image.PreparationCommand, []string{PreparationPath}):
		return nil, fmt.Errorf(
			"project-image preparation command %q is not the approved %q: %w",
			image.PreparationCommand, PreparationPath, ErrComposition)
	}
	return slices.Clone(image.PreparationCommand), nil
}
