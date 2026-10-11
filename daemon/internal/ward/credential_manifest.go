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
//
// authorize durably records the observer container's name in the caller's
// runtime authority before any container is created, so a preflight killed
// after creation but before the in-process reap leaves nothing the authority's
// cleanup and stale recovery cannot enumerate. It is nil-tolerant for callers
// with no such authority; the production preflight always passes the rig binder.
func InspectCredentialVolumeManifest(
	ctx context.Context,
	runtime Runtime,
	exporterImage, volume string,
	manifest CredentialManifestPolicy,
	authorize RuntimeResourceAuthorizer,
) error {
	_, err := observeCredentialVolume(ctx, runtime, exporterImage, volume, manifest, authorize)
	return err
}

// observeCredentialVolume is InspectCredentialVolumeManifest's observation,
// also returning the hex SHA-256 the exporter proof binds over the volume's
// complete tree.
func observeCredentialVolume(
	ctx context.Context,
	runtime Runtime,
	exporterImage, volume string,
	manifest CredentialManifestPolicy,
	authorize RuntimeResourceAuthorizer,
) (string, error) {
	proof, _, err := observeCredentialVolumeProof(
		ctx, runtime, exporterImage, volume, manifest, authorize, credObserverGate)
	return proof.tree, err
}

// observeCredentialVolumeProof runs one credential observer over the volume
// and returns its parsed proof, with the token a token-read observer read;
// observer selects the script (observeCredentialProof).
func observeCredentialVolumeProof(
	ctx context.Context,
	runtime Runtime,
	exporterImage, volume string,
	manifest CredentialManifestPolicy,
	authorize RuntimeResourceAuthorizer,
	observer credObserver,
) (credProof, SetupToken, error) {
	fail := func(err error) (credProof, SetupToken, error) { return credProof{}, SetupToken{}, err }
	if runtime == nil || exporterImage == "" || volume == "" || !manifest.valid() {
		return fail(errors.New("credential manifest inspection requires a runtime, exporter image, volume, and policy"))
	}
	cfg := (Config{ExporterImage: exporterImage}).withDefaults()
	owner, err := newOwnershipLabel()
	if err != nil {
		return fail(err)
	}
	runID := "preflight-" + owner.Value[:12]
	name := "freeside-preflight-credential-" + owner.Value[:12]
	if authorize != nil {
		if err := authorize(ctx, RuntimeResourceNames{Containers: []string{name}}); err != nil {
			return fail(err)
		}
	}
	handoff := HandoffSpec{
		RunID: runID,
		Agent: AgentSpec{CredentialMounts: []CredentialMount{{
			Volume: volume, Target: "/credentials", Manifest: manifest,
		}}},
		AuthStoreLease: &AuthStoreLeaseClaim{},
		Class:          LaunchConformance,
		Size:           DefaultLaunchSize(LaunchConformance),
	}
	backend := &Backend{
		rt: runtime, cfg: cfg, runtimeOps: newRuntimeOps(runtime, cfg), initialized: true,
	}
	state := &runState{ownershipLabel: owner}
	var claim objectClaim
	proof, token, inspectErr := backend.observeCredentialProof(ctx, handoff, name, state, &claim, observer)
	if inspectErr == nil {
		return proof, token, nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), max(cfg.TeardownTimeout, time.Second))
	defer cancel()
	cleanupErr := backend.runtimeOps.reapUnlistedContainer(cleanupCtx, name, claim, owner)
	return fail(errors.Join(inspectErr, cleanupErr))
}
