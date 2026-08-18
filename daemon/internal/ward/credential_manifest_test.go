package ward

import (
	"context"
	"errors"
	"strings"
	"testing"
)

//nolint:gosec // G101 false positive: a fixture volume name, not a credential value.
const preflightCredentialVolume = "freeside-preflight-credential-volume-fixture"

// TestInspectCredentialVolumeManifestAuthorizesBeforeCreate proves the probe
// records its observer container's exact name in the runtime authority before
// it touches the runtime, so a preflight killed after creation leaves a name
// the authority's cleanup can enumerate. The setup-token proof the fake renders
// omits the manifest line, so the probe itself fails; the authorization
// ordering is what this test pins.
func TestInspectCredentialVolumeManifestAuthorizesBeforeCreate(t *testing.T) {
	f := newFakeRuntime(t)
	var (
		authorized  int
		callsAtAuth int
		gotNames    RuntimeResourceNames
	)
	authorize := func(_ context.Context, names RuntimeResourceNames) error {
		authorized++
		callsAtAuth = f.callCount()
		gotNames = names
		return nil
	}
	err := InspectCredentialVolumeManifest(
		context.Background(), f, "registry.test/exporter", preflightCredentialVolume,
		CredentialManifestSetupToken, authorize,
	)
	if err == nil {
		t.Fatal("probe unexpectedly succeeded against the fake runtime")
	}
	if authorized != 1 {
		t.Fatalf("authorize called %d times, want exactly 1", authorized)
	}
	if callsAtAuth != 0 {
		t.Fatalf("authorize ran after %d runtime calls, want 0 (before any create)", callsAtAuth)
	}
	if len(gotNames.Containers) != 1 || len(gotNames.Volumes) != 0 || len(gotNames.Networks) != 0 {
		t.Fatalf("authorized names = %+v, want exactly one container and nothing else", gotNames)
	}
	name := gotNames.Containers[0]
	if !strings.HasPrefix(name, "freeside-preflight-credential-") {
		t.Fatalf("authorized name %q lacks the preflight credential prefix", name)
	}
	if f.callIndex("create-container "+name) < 0 {
		t.Fatalf("observer container %q was never created after authorization", name)
	}
}

// TestInspectCredentialVolumeManifestStopsOnAuthorizationFailure proves a
// refused authorization creates nothing: the probe returns the refusal before
// any container exists, so there is no unrecorded observer to strand.
func TestInspectCredentialVolumeManifestStopsOnAuthorizationFailure(t *testing.T) {
	f := newFakeRuntime(t)
	sentinel := errors.New("rig bind refused")
	err := InspectCredentialVolumeManifest(
		context.Background(), f, "registry.test/exporter", preflightCredentialVolume,
		CredentialManifestSetupToken,
		func(context.Context, RuntimeResourceNames) error { return sentinel },
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("probe error = %v, want the authorizer refusal", err)
	}
	if f.callCount() != 0 {
		t.Fatalf("authorization failure still drove %d runtime calls", f.callCount())
	}
}

// TestInspectCredentialVolumeManifestReapsAfterFailedProbe proves the
// in-process cleanup still deletes the observer container when the probe fails
// after creation, so a completed (non-interrupted) failed probe strands
// nothing.
func TestInspectCredentialVolumeManifestReapsAfterFailedProbe(t *testing.T) {
	f := newFakeRuntime(t)
	err := InspectCredentialVolumeManifest(
		context.Background(), f, "registry.test/exporter", preflightCredentialVolume,
		CredentialManifestSetupToken,
		func(context.Context, RuntimeResourceNames) error { return nil },
	)
	if err == nil {
		t.Fatal("probe unexpectedly succeeded against the fake runtime")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ctrs) != 0 {
		t.Fatalf("failed probe left %d container(s) behind", len(f.ctrs))
	}
	created, deleted := 0, 0
	for _, c := range f.calls {
		switch {
		case strings.HasPrefix(c, "create-container "):
			created++
		case strings.HasPrefix(c, "delete-container "):
			deleted++
		}
	}
	if created == 0 || deleted == 0 {
		t.Fatalf("create=%d delete=%d, want the observer created then reaped", created, deleted)
	}
}
