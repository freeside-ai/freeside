package ward

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestLiveSetupTokenRead runs the token-read observer in the reference
// runtime with the real exporter image, whose shell, head, and rootfs export
// the host-shell unit tests cannot stand in for. It pins that the read hands
// back the bytes a launch would load, under the digests the integrity
// observation reports for the same volume; that a file the daemon would not
// accept is refused with no token; and that the read leaves no observer
// behind. The stored value is a made-up fixture, not a credential.
//
//	FREESIDE_WARD_LIVE_TEST=1 FREESIDE_WARD_EXPORTER_IMAGE=<digest-ref> \
//	  go test ./internal/ward -run TestLiveSetupTokenRead -v
func TestLiveSetupTokenRead(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("live credential-read test skipped: set FREESIDE_WARD_LIVE_TEST=1 and FREESIDE_WARD_EXPORTER_IMAGE (requires macOS, Apple container 1.1.0, and `container system start`)")
	}
	exporter := liveExporterImage(t)
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	stamp := time.Now().Unix()
	volume := fmt.Sprintf("freeside-ward-live-read-%d", stamp)
	seedName := fmt.Sprintf("freeside-ward-live-read-seed-%d", stamp)
	t.Cleanup(func() {
		_ = rt.StopContainer(ctx, seedName)
		_ = rt.DeleteContainer(ctx, seedName)
		_ = rt.DeleteVolume(ctx, volume)
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
	observers := func() []string {
		t.Helper()
		containers, err := rt.ListContainers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range containers {
			if strings.HasPrefix(c.ID, "freeside-preflight-credential-") {
				names = append(names, c.ID)
			}
		}
		return names
	}
	before := observers()
	sum := func(body string) domain.Digest { return domain.Digest(contentaddr.Sum([]byte(body))) }

	seed("rm -rf /cred/lost+found && printf %s " + integritySentinelToken + " > /cred/token && chmod 0400 /cred/token")
	read, err := ReadSetupToken(ctx, rt, exporter, volume, nil)
	if err != nil {
		t.Fatalf("ReadSetupToken = %v", err)
	}
	if string(read.Token.Reveal()) != integritySentinelToken {
		t.Error("the read did not hand back the stored token")
	}
	seen, err := ObserveSetupTokenIntegrity(ctx, rt, exporter, volume, nil)
	if err != nil {
		t.Fatalf("ObserveSetupTokenIntegrity = %v", err)
	}
	if read.TokenDigest != sum(integritySentinelToken) || read.TokenDigest != seen.TokenDigest ||
		read.TreeDigest != seen.TreeDigest || seen.Truncated {
		t.Errorf("read digests = %s, %s; the integrity observation saw %+v", read.TokenDigest, read.TreeDigest, seen)
	}

	// A hand-written file ends in a newline, which a launch drops.
	seed("printf '%s\\n' " + integritySentinelToken + " > /cred/token")
	read, err = ReadSetupToken(ctx, rt, exporter, volume, nil)
	if err != nil || string(read.Token.Reveal()) != integritySentinelToken ||
		read.TokenDigest != sum(integritySentinelToken+"\n") {
		t.Errorf("newline-terminated token = %v, digest %s; want the token without its line ending", err, read.TokenDigest)
	}

	for name, script := range map[string]string{
		"too long":          "head -c " + fmt.Sprint(MaxSetupTokenBytes+2) + " /dev/zero | tr '\\0' a > /cred/token",
		"not a setup token": "printf %s " + liveMarker + liveMarker + " > /cred/token",
		"empty":             ": > /cred/token",
	} {
		seed(script)
		read, err := ReadSetupToken(ctx, rt, exporter, volume, nil)
		if err == nil || len(read.Token.Reveal()) != 0 {
			t.Errorf("%s token file = %v, want a refusal and no token", name, err)
		}
	}

	seed("rm /cred/token")
	if _, err := ReadSetupToken(ctx, rt, exporter, volume, nil); !errors.Is(err, ErrCredentialStoreAbsent) {
		t.Errorf("token-less volume = %v, want ErrCredentialStoreAbsent", err)
	}

	if after := observers(); len(after) != len(before) {
		t.Errorf("observer containers after the reads = %v, before = %v; a read left one behind", after, before)
	}
}
