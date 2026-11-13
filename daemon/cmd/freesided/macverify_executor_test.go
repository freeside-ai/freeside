package main

import (
	"context"
	"errors"
	"flag"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var errUnexpectedProve = errors.New("the conformance pass ran without -run-conformance")

func refuseProve(context.Context, *macverify.Backend, macverify.ConformanceRecorder) error {
	return errUnexpectedProve
}

func parseHostVerificationFlags(t *testing.T, args ...string) daemoncompose.MacverifyExecutorConfig {
	t.Helper()
	return parseHostVerificationFlagsListening(t, "127.0.0.1:0", args...)
}

func parseHostVerificationFlagsListening(t *testing.T, listen string, args ...string) daemoncompose.MacverifyExecutorConfig {
	t.Helper()
	flags := flag.NewFlagSet("freesided", flag.ContinueOnError)
	config := macverifyExecutorFlags(flags)
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return config(listen)
}

func hostVerificationFlags(root string) []string {
	return []string{
		"-macverify-root", root, "-macverify-slots", "fvslot2,fvslot1",
		"-macverify-xcode-build", "17F42", "-macverify-swift", "swiftlang-6.1.2.1.2",
	}
}

func recordHostVerificationConformance(
	t *testing.T, st *store.Store, outcome domain.ConformanceOutcome, digest domain.Digest,
) {
	t.Helper()
	in := domain.BackendConformanceInput{
		Backend: domain.BackendCredentialFreeHostVerification, Outcome: outcome,
		ConfigurationDigest: digest, ProvedAt: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
	}
	if outcome == domain.ConformancePassed {
		in.Capabilities = domain.NewCapabilitySnapshot(domain.CapCredentialFreeHostVerification)
	}
	record, err := domain.NewBackendConformance(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := (daemoncompose.StoreConformanceRecorder{Store: st}).RecordBackendConformance(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

func declaresHostVerification(b *macverify.Backend) bool {
	return b.Capabilities().Has(exec.CapCredentialFreeHostVerification)
}

// TestHostVerificationExecutorUnsetConfiguresNone: with no root the daemon
// starts with no executor and never reads the store or runs a pass.
func TestHostVerificationExecutorUnsetConfiguresNone(t *testing.T) {
	t.Parallel()
	cfg := parseHostVerificationFlags(t)
	backend, err := daemoncompose.ComposeHostVerificationExecutor(context.Background(), nil, cfg, true, nil, refuseProve)
	if err != nil || backend != nil {
		t.Fatalf("unset root = %v, %v; want no executor", backend, err)
	}
}

// TestHostVerificationExecutorBuildsFromFlags: the flags become the
// backend's configuration, slots sorted, so its digest is the one the
// install's configuration proves.
func TestHostVerificationExecutorBuildsFromFlags(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "macverify")
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	backend, err := daemoncompose.ComposeHostVerificationExecutor(
		context.Background(), st, parseHostVerificationFlags(t, hostVerificationFlags(root)...), false, nil, refuseProve)
	if err != nil {
		t.Fatal(err)
	}
	slots := []string{"fvslot1", "fvslot2"}
	want, err := macverify.New(macverify.Config{
		ExecutorRoot: root, SandboxProfile: macverify.DefaultSandboxProfile(),
		PacketFilterRules: macverify.PacketFilterRules(slots), Slots: slots,
		Toolchain: macverify.Toolchain{XcodeBuild: "17F42", Swift: "swiftlang-6.1.2.1.2"},
		PortBase:  macverify.DefaultPortBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.ConfigurationDigest() != want.ConfigurationDigest() || declaresHostVerification(backend) {
		t.Fatalf("backend digest %s declared %v, want %s undeclared",
			backend.ConfigurationDigest(), declaresHostVerification(backend), want.ConfigurationDigest())
	}

	missingToolchain := parseHostVerificationFlags(t, "-macverify-root", root)
	if _, err := daemoncompose.ComposeHostVerificationExecutor(
		context.Background(), st, missingToolchain, false, nil, refuseProve,
	); !errors.Is(err, macverify.ErrInvalidConfig) {
		t.Fatalf("root without a toolchain pin = %v, want ErrInvalidConfig", err)
	}
}

// TestHostVerificationExecutorRefusesAReservedListenPort: the daemon may not
// listen on a port a slot reserves, or a job could reach its API. Two slots
// from base 47000 reserve 47000 to 47007; the port base is in the digest.
func TestHostVerificationExecutorRefusesAReservedListenPort(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "macverify")
	compose := func(listen string, extra ...string) (*macverify.Backend, error) {
		st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
		cfg := parseHostVerificationFlagsListening(t, listen, append(hostVerificationFlags(root), extra...)...)
		return daemoncompose.ComposeHostVerificationExecutor(context.Background(), st, cfg, false, nil, refuseProve)
	}
	for _, listen := range []string{"127.0.0.1:47000", "127.0.0.1:47007", "[::1]:47004"} {
		if _, err := compose(listen); err == nil {
			t.Errorf("listen %s = nil, want a refusal", listen)
		}
	}
	base, err := compose("127.0.0.1:47008")
	if err != nil {
		t.Fatalf("listen past the last reserved port = %v", err)
	}
	if _, err := compose("127.0.0.1:47000", "-macverify-port-base", "46000"); err != nil {
		t.Fatalf("listen outside a moved reservation = %v", err)
	}
	moved, err := compose("127.0.0.1:0", "-macverify-port-base", "46000")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ConfigurationDigest() == base.ConfigurationDigest() {
		t.Error("the port base does not change the configuration digest")
	}
}

// TestHostVerificationExecutorRestoresOnlyItsOwnPass: a passed record for
// the current configuration restores the declaration (on macOS, the only
// host it can vouch for); a record for another configuration or a failed
// one leaves the backend undeclared without stopping startup.
func TestHostVerificationExecutorRestoresOnlyItsOwnPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "macverify")
	cfg := parseHostVerificationFlags(t, hostVerificationFlags(root)...)
	probe, err := daemoncompose.ComposeHostVerificationExecutor(ctx, storetest.Open(t, filepath.Join(t.TempDir(), "probe.db"), store.Options{}), cfg, false, nil, refuseProve)
	if err != nil {
		t.Fatal(err)
	}
	current := probe.ConfigurationDigest()
	other := domain.Digest("sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	for name, tc := range map[string]struct {
		outcome domain.ConformanceOutcome
		digest  domain.Digest
		declare bool
	}{
		"passed for the current configuration": {domain.ConformancePassed, current, runtime.GOOS == "darwin"},
		"passed for another configuration":     {domain.ConformancePassed, other, false},
		"failed for the current configuration": {domain.ConformanceFailed, current, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
			recordHostVerificationConformance(t, st, tc.outcome, tc.digest)
			backend, err := daemoncompose.ComposeHostVerificationExecutor(ctx, st, cfg, false, nil, refuseProve)
			if err != nil {
				t.Fatal(err)
			}
			if got := declaresHostVerification(backend); got != tc.declare {
				t.Fatalf("declared = %v, want %v", got, tc.declare)
			}
		})
	}
}

// TestHostVerificationExecutorRunsTheFullPass: -run-conformance hands the
// backend and the store's recorder to the pass, and a failed pass fails
// startup.
func TestHostVerificationExecutorRunsTheFullPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	cfg := parseHostVerificationFlags(t, hostVerificationFlags(filepath.Join(t.TempDir(), "macverify"))...)
	var proved *macverify.Backend
	backend, err := daemoncompose.ComposeHostVerificationExecutor(ctx, st, cfg, true, nil,
		func(ctx context.Context, b *macverify.Backend, recorder macverify.ConformanceRecorder) error {
			proved = b
			record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
				Backend: domain.BackendCredentialFreeHostVerification, Outcome: domain.ConformancePassed,
				ConfigurationDigest: b.ConfigurationDigest(),
				Capabilities:        domain.NewCapabilitySnapshot(domain.CapCredentialFreeHostVerification),
				ProvedAt:            time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
			})
			if err != nil {
				return err
			}
			return recorder.RecordBackendConformance(ctx, record)
		})
	if err != nil || backend == nil || proved != backend {
		t.Fatalf("run conformance = %v, %v; the pass ran on %v", backend, err, proved)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		record, found, err := tx.LatestBackendConformance(ctx, domain.BackendCredentialFreeHostVerification)
		if err == nil && (!found || record.ConfigurationDigest != backend.ConfigurationDigest()) {
			t.Errorf("recorded pass = %#v (found %v), want one for %s", record, found, backend.ConfigurationDigest())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	failed := errors.New("slot reached the network")
	if _, err := daemoncompose.ComposeHostVerificationExecutor(ctx, st, cfg, true, nil,
		func(context.Context, *macverify.Backend, macverify.ConformanceRecorder) error { return failed },
	); !errors.Is(err, failed) {
		t.Fatalf("failed pass = %v, want startup to fail with it", err)
	}
}
