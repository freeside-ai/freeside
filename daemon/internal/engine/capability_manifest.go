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

// offerableManifests returns the capability choices a failed writer attempt's
// card may offer: each policy manifest that names a profile other than the one
// the attempt was admitted under and that the retry's admission would not
// refuse. The retry inherits this run's policy keys, so a choice refused here
// could only allocate a run that holds at admission and never clears. That
// covers a profile the composition does not enforce and a provider_registry
// manifest under a policy with no valid declared set. A policy whose
// manifests do not reconstruct offers nothing.
func offerableManifests(
	enforceable []domain.EgressProfile, policy domain.ResolvedPolicy, admitted domain.EgressProfile,
) []domain.CapabilityManifestOffer {
	manifests, err := domain.CapabilityManifestsFromPolicy(policy)
	if err != nil {
		return nil
	}
	var offers []domain.CapabilityManifestOffer
	for _, manifest := range manifests {
		if manifest.EgressProfile == admitted ||
			writerEgressRefusal(enforceable, policy, manifest.EgressProfile) != nil {
			continue
		}
		offers = append(offers, manifest.Offer())
	}
	return offers
}
