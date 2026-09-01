package engine

import (
	"context"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func (e *Engine) capabilityManifestForRun(
	ctx context.Context, run domain.Run, digest domain.Digest,
) (domain.CapabilityManifest, error) {
	if e.admission == nil {
		return domain.CapabilityManifest{}, fmt.Errorf("capability manifest admission is not configured")
	}
	var policy domain.ResolvedPolicy
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		policy, err = tx.GetResolvedPolicy(ctx, run.ID)
		return err
	}); err != nil {
		return domain.CapabilityManifest{}, err
	}
	manifests, err := domain.CapabilityManifestsFromPolicy(policy)
	if err != nil {
		return domain.CapabilityManifest{}, err
	}
	for _, manifest := range manifests {
		if manifest.Digest != digest {
			continue
		}
		if !slices.Contains(e.admission.environment.EnforceableEgressProfiles, manifest.EgressProfile) {
			return domain.CapabilityManifest{}, fmt.Errorf(
				"manifest %q egress profile %q is not enforceable: %w",
				manifest.Name, manifest.EgressProfile, domain.ErrCapabilityManifestInvalid)
		}
		return manifest, nil
	}
	return domain.CapabilityManifest{}, fmt.Errorf(
		"manifest digest %q is absent from current run policy: %w",
		digest, domain.ErrCapabilityManifestInvalid)
}
