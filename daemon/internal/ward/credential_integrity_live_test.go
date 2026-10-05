package ward

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestLiveSetupTokenIntegrityObservation runs the integrity observer in the
// reference runtime with the real exporter image, whose shell and coreutils
// the host-shell unit tests cannot stand in for. It pins three things: the
// tree digest equals the one auth adopt records for the same volume, the
// token digest and length verdict follow the token's bytes, and a missing
// volume or token is reported as absent without the runtime creating one.
//
//	FREESIDE_WARD_LIVE_TEST=1 FREESIDE_WARD_EXPORTER_IMAGE=<digest-ref> \
//	  go test ./internal/ward -run TestLiveSetupTokenIntegrityObservation -v
func TestLiveSetupTokenIntegrityObservation(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("live credential-integrity test skipped: set FREESIDE_WARD_LIVE_TEST=1 and FREESIDE_WARD_EXPORTER_IMAGE (requires macOS, Apple container 1.1.0, and `container system start`)")
	}
	exporter := liveExporterImage(t)
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	stamp := time.Now().Unix()
	volume := fmt.Sprintf("freeside-ward-live-integrity-%d", stamp)
	missing := volume + "-missing"
	seedName := fmt.Sprintf("freeside-ward-live-integrity-seed-%d", stamp)
	t.Cleanup(func() {
		_ = rt.StopContainer(ctx, seedName)
		_ = rt.DeleteContainer(ctx, seedName)
		_ = rt.DeleteVolume(ctx, volume)
		_ = rt.DeleteVolume(ctx, missing)
	})
	if err := rt.CreateVolume(ctx, volume, 8, []Label{{Key: "freeside.ward-live", Value: volume}}); err != nil {
		t.Fatalf("create credential volume: %v", err)
	}
	// seed rewrites the volume with one networkless container run.
	seed := func(script string) {
		t.Helper()
		if err := rt.CreateContainer(ctx, ContainerSpec{
			Name: seedName, Image: exporter, Size: testContainerSize,
			Command:         []string{"sh", "-c", script},
			Mounts:          []Mount{{Type: MountVolume, Source: volume, Target: "/cred"}},
			NetworkDisabled: true,
		}); err != nil {
			t.Fatalf("create seed container: %v", err)
		}
		if err := rt.StartContainer(ctx, seedName); err != nil {
			t.Fatalf("start seed container: %v", err)
		}
		waitLiveStopped(t, rt, seedName)
		if err := rt.DeleteContainer(ctx, seedName); err != nil {
			t.Fatalf("delete seed container: %v", err)
		}
	}
	observe := func() SetupTokenIntegrity {
		t.Helper()
		seen, err := ObserveSetupTokenIntegrity(ctx, rt, exporter, volume, nil)
		if err != nil {
			t.Fatalf("ObserveSetupTokenIntegrity = %v", err)
		}
		return seen
	}
	sum := func(body string) domain.Digest { return domain.Digest(contentaddr.Sum([]byte(body))) }

	// The setup-token manifest admits exactly one entry, so the
	// filesystem's own lost+found goes first.
	seed("rm -rf /cred/lost+found && printf " + liveMarker + " > /cred/token && chmod 0400 /cred/token")
	adopted, err := ObserveSetupTokenVolume(rt, exporter, nil)(ctx, volume)
	if err != nil {
		t.Fatalf("adoption observation = %v", err)
	}
	intact := observe()
	if intact.TreeDigest != adopted {
		t.Errorf("tree digest = %s, want the digest adoption records, %s", intact.TreeDigest, adopted)
	}
	if intact.TokenDigest != sum(liveMarker) || intact.Truncated {
		t.Errorf("intact token = %+v, want the marker's digest and no truncation", intact)
	}
	if again := observe(); again != intact {
		t.Errorf("a second observation = %+v, want %+v: the observer changed the store", again, intact)
	}

	const short = "short"
	seed("printf " + short + " > /cred/token")
	cut := observe()
	if cut.TokenDigest != sum(short) || !cut.Truncated || cut.TreeDigest == adopted {
		t.Errorf("short token = %+v, want its digest, a truncation verdict, and a moved tree digest", cut)
	}

	seed(": > /cred/token")
	if empty := observe(); empty.TokenDigest != sum("") || !empty.Truncated {
		t.Errorf("empty token = %+v, want the empty digest and a truncation verdict", empty)
	}

	seed("rm /cred/token")
	if _, err := ObserveSetupTokenIntegrity(ctx, rt, exporter, volume, nil); !errors.Is(err, ErrCredentialStoreAbsent) {
		t.Errorf("token-less volume = %v, want ErrCredentialStoreAbsent", err)
	}

	if _, err := ObserveSetupTokenIntegrity(ctx, rt, exporter, missing, nil); !errors.Is(err, ErrCredentialStoreAbsent) {
		t.Errorf("missing volume = %v, want ErrCredentialStoreAbsent", err)
	}
	volumes, err := rt.ListVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range volumes {
		if v.Name == missing {
			t.Errorf("observing a missing volume created %s", missing)
		}
	}
}
