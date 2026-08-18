package ward

import (
	"context"
	"errors"
	"time"
)

// InspectCredentialVolumeManifest observes an existing credential volume
// through the same networkless, read-only exporter proof and manifest parser
// used immediately before a handoff writer starts. It never returns credential
// bytes or a content digest.
func InspectCredentialVolumeManifest(
	ctx context.Context,
	runtime Runtime,
	exporterImage, volume string,
	manifest CredentialManifestPolicy,
) error {
	if runtime == nil || exporterImage == "" || volume == "" || !manifest.valid() {
		return errors.New("credential manifest inspection requires a runtime, exporter image, volume, and policy")
	}
	cfg := (Config{ExporterImage: exporterImage}).withDefaults()
	owner, err := newOwnershipLabel()
	if err != nil {
		return err
	}
	runID := "preflight-" + owner.Value[:12]
	name := "freeside-preflight-credential-" + owner.Value[:12]
	handoff := HandoffSpec{
		RunID: runID,
		Agent: AgentSpec{CredentialMounts: []CredentialMount{{
			Volume: volume, Target: "/credentials", Manifest: manifest,
		}}},
		AuthStoreLease: &AuthStoreLeaseClaim{},
	}
	backend := &Backend{
		rt: runtime, cfg: cfg, runtimeOps: newRuntimeOps(runtime, cfg), initialized: true,
	}
	state := &runState{ownershipLabel: owner}
	var claim objectClaim
	_, inspectErr := backend.observeCredentialStore(ctx, handoff, name, state, &claim)
	if inspectErr == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), max(cfg.TeardownTimeout, time.Second))
	defer cancel()
	cleanupErr := backend.runtimeOps.reapUnlistedContainer(cleanupCtx, name, claim, owner)
	return errors.Join(inspectErr, cleanupErr)
}
