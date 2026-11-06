package wardstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// codexProbeAuthStore writes a live Codex auth.json, whose access token
// outlives the probe floor at the rig's clock, under a fresh private root.
func codexProbeAuthStore(t *testing.T, r *credentialReadRig) (root, path string) {
	t.Helper()
	root = t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // fixture is a private directory
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": "operator-id", "access_token": codexTestJWT(r.clock.Add(2 * time.Hour)),
			"refresh_token": "operator-refresh-SECRET", "account_id": "acct-codex-main",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(root, "auth.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func codexProbeReader(r *credentialReadRig, root string) wardstore.CodexProbeCredentialReader {
	return wardstore.CodexProbeCredentialReader{Store: r.st, InputRoot: root, Now: func() time.Time { return r.clock }}
}

func TestCodexProbeCredentialReaderReadsTheCurrentStoreUnderAHold(t *testing.T) {
	ctx := context.Background()
	r := newCredentialReadRig(t)
	root, path := codexProbeAuthStore(t, r)
	enrollment := r.enrollCodex("codex-main", path, probeDigest("store"))
	credential, err := codexProbeReader(r, root).Read(ctx, enrollment)
	if err != nil {
		t.Fatal(err)
	}
	// The credential carries this store's access token: output echoing it is
	// refused. The ward's own tests pin the refresh-token strip.
	if _, err := ward.DecodeCodexModelReport(ward.LineupProbeOutput{
		Stdout: []byte(codexTestJWT(r.clock.Add(2 * time.Hour))),
	}, credential); !errors.Is(err, ward.ErrModelReportCarriesCredential) {
		t.Fatalf("output echoing the store's access token: err = %v", err)
	}
	// The hold was released: a mutation can start right after the read.
	if _, err := r.adapters.Leaser.Acquire(ctx, "codex-main", "mutator", r.clock, r.clock.Add(time.Minute)); err != nil {
		t.Fatalf("a mutation cannot start after the read, so the hold was not released: %v", err)
	}
}

func TestCodexProbeCredentialReaderRefuses(t *testing.T) {
	ctx := context.Background()

	t.Run("not a Codex OAuth enrollment", func(t *testing.T) {
		r := newCredentialReadRig(t)
		root, _ := codexProbeAuthStore(t, r)
		claude := r.enrollClaude(readIdentity, probeDigest("token"))
		if _, err := codexProbeReader(r, root).Read(ctx, claude); !errors.Is(err, wardstore.ErrCodexProbeCredentialUnsupported) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("live mutation lease", func(t *testing.T) {
		r := newCredentialReadRig(t)
		root, path := codexProbeAuthStore(t, r)
		enrollment := r.enrollCodex("codex-main", path, probeDigest("store"))
		if _, err := r.adapters.Leaser.Acquire(ctx, "codex-main", "refresh", r.clock, r.clock.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := codexProbeReader(r, root).Read(ctx, enrollment); !errors.Is(err, wardstore.ErrCredentialReadMutationLeaseLive) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a store outside the input root", func(t *testing.T) {
		r := newCredentialReadRig(t)
		_, path := codexProbeAuthStore(t, r)
		other, _ := codexProbeAuthStore(t, r)
		enrollment := r.enrollCodex("codex-main", path, probeDigest("store"))
		if _, err := codexProbeReader(r, other).Read(ctx, enrollment); err == nil {
			t.Fatal("read a store outside the input root")
		}
		if _, err := r.adapters.Leaser.Acquire(ctx, "codex-main", "mutator", r.clock, r.clock.Add(time.Minute)); err != nil {
			t.Fatalf("a refused read kept its hold: %v", err)
		}
	})

	t.Run("an access token near expiry", func(t *testing.T) {
		r := newCredentialReadRig(t)
		root, path := codexProbeAuthStore(t, r)
		enrollment := r.enrollCodex("codex-main", path, probeDigest("store"))
		r.clock = r.clock.Add(2*time.Hour - 5*time.Minute)
		if _, err := codexProbeReader(r, root).Read(ctx, enrollment); err == nil {
			t.Fatal("read a token that expires within the floor")
		}
		if _, err := r.adapters.Leaser.Acquire(ctx, "codex-main", "mutator", r.clock, r.clock.Add(time.Minute)); err != nil {
			t.Fatalf("a refused read kept its hold: %v", err)
		}
	})

	t.Run("a read that outlives its hold", func(t *testing.T) {
		r := newCredentialReadRig(t)
		root, path := codexProbeAuthStore(t, r)
		enrollment := r.enrollCodex("codex-main", path, probeDigest("store"))
		reader := codexProbeReader(r, root)
		// Each reading of the clock is 20 seconds after the last, so the
		// check after the store read falls past the 30-second hold.
		at := r.clock
		reader.Now = func() time.Time {
			at = at.Add(20 * time.Second)
			return at
		}
		if _, err := reader.Read(ctx, enrollment); !errors.Is(err, wardstore.ErrCredentialReadHoldEnded) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("missing dependencies", func(t *testing.T) {
		if _, err := (wardstore.CodexProbeCredentialReader{}).Read(ctx, "codex-main/codex_cli"); err == nil {
			t.Fatal("read without a store")
		}
	})
}
