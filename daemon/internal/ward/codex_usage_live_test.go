package ward

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A made-up provider answer for the synthetic cases. Their tokens are
// fabricated, so nothing they send may reach the provider, and what comes
// back proves only that the read completed, never that the provider answers.
const codexUsageSyntheticBody = `{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,` +
	`"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_after_seconds":120,"reset_at":1735689720},` +
	`"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_after_seconds":43200,"reset_at":1735693200}}}`

func codexUsageSyntheticUpstream(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(codexUsageSyntheticBody)),
	}, nil
}

// CI deliberately skips the experiment. The synthetic cases measure refresh
// behavior only; the real cases need the operator's auth store and consent to
// one inference turn, and without them the overall result is never pass.
func TestLiveCodexUsage(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_WARD_LIVE_TEST=1; see Codex Usage Spike in daemon/README.md")
	}
	image := os.Getenv("FREESIDE_WARD_CODEX_AGENT_IMAGE")
	if !digestPinnedImagePattern.MatchString(image) {
		t.Fatal("FREESIDE_WARD_CODEX_AGENT_IMAGE must be digest pinned")
	}
	privateCapture := os.Getenv("FREESIDE_WARD_USAGE_PRIVATE_CAPTURE_DIR")
	if privateCapture != "" {
		info, err := os.Stat(privateCapture) //nolint:gosec // explicit operator-selected private output directory, not provider input
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			t.Fatal("private capture directory must exist and allow only its owner")
		}
		entries, err := os.ReadDir(privateCapture)
		if err != nil || len(entries) != 0 {
			t.Fatal("private capture directory must be empty")
		}
	}
	bin, err := exec.LookPath("container")
	if err != nil {
		t.Fatal(err)
	}
	rt := NewCLIRuntime(bin)
	verdicts := map[string]string{}
	report := func(t *testing.T, evidence codexUsageEvidence) {
		t.Helper()
		evidence.Verdict = codexUsageAnalyze(evidence)
		verdicts[evidence.Case] = evidence.Verdict
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(encoded))
		// A deferred launch is the gate working, and a turn that completes
		// without a notification is an observation; neither is a broken run.
		if evidence.Verdict == "fail" || evidence.Verdict == "probe_failed" {
			t.Errorf("%s: %s case", evidence.Verdict, evidence.Case)
		}
	}
	for name, lifetime := range map[string]time.Duration{"synthetic_fresh": time.Hour, "synthetic_near_expiry": 2 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			evidence := codexUsageRun(t, rt, bin, image, name, "idle", codexAccountSynthetic(t, lifetime), nil, privateCapture)
			evidence.Credential, evidence.Gate, evidence.HostStore = "synthetic", "not_applied", "not_read"
			report(t, evidence)
		})
	}
	store := os.Getenv("FREESIDE_WARD_CODEX_AUTH_STORE")
	for _, mode := range []string{"idle", "turn"} {
		t.Run("real_"+mode, func(t *testing.T) {
			if store == "" {
				t.Skip("set FREESIDE_WARD_CODEX_AUTH_STORE to a subscription auth.json; without it there is no provider observation")
			}
			// The host store is only ever read, through the production
			// private-file checks. Its refresh token never leaves this process.
			read := func() ([]byte, [sha256.Size]byte) {
				// The operator's path is admitted by the production checks,
				// which compare it with its resolved form.
				path := filepath.Clean(store)
				_, body, err := readCodexReviewInput(filepath.Dir(path), path, maxCodexAuthSnapshotBytes)
				if err != nil {
					t.Fatal("auth store must be an owner-only, singly linked regular file in an owner-only directory")
				}
				return body, sha256.Sum256(body)
			}
			host, before := read()
			snapshot, _, err := codexReviewAgentAuthSnapshot(CodexAuthSubscription, host)
			if err != nil {
				t.Fatal("auth store is not a subscription store the production derivation accepts")
			}
			expires, err := inspectCodexAuthSnapshot(CodexAuthSubscription, snapshot)
			if err != nil {
				t.Fatal("derived snapshot failed the production inspection")
			}
			evidence := codexUsageRun(t, rt, bin, image, "real_"+mode, mode, snapshot, func() bool {
				admitted, remaining := codexUsageLaunchGate(expires, time.Now())
				t.Logf("gate: admitted=%t remaining=%s floor=%s", admitted, remaining.Truncate(time.Minute), codexUsageLaunchFloor)
				return admitted
			}, privateCapture)
			evidence.Credential, evidence.HostStore = "real", "unchanged"
			if _, after := read(); after != before {
				evidence.HostStore = "changed"
			}
			report(t, evidence)
		})
	}
	t.Logf("overall=%s (pass needs both synthetic cases and both real cases)", codexUsageOverall(verdicts))
}

// codexUsageRun launches one bounded probe. gate is nil for the synthetic
// experiment, whose made-up requests never reach the provider. For a real
// credential it is consulted before any resource exists and again
// immediately before the driver starts; a refusal returns a deferred result
// without starting the app-server.
func codexUsageRun(t *testing.T, rt *CLIRuntime, bin, image, name, mode string, body []byte, gate func() bool, privateCapture string) codexUsageEvidence {
	t.Helper()
	deferred := codexUsageEvidence{Case: name, Gate: "deferred", Requests: []string{}}
	var upstream http.RoundTripper
	if gate == nil {
		upstream = codexAuthRoundTripFunc(codexUsageSyntheticUpstream)
	} else if !gate() {
		return deferred
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	prefix := "freeside-codex-usage-" + hex.EncodeToString(random[:])
	observer, network := prefix+"-observer", prefix+"-egress"
	recovery, err := os.MkdirTemp("", "freeside-codex-usage-recovery-")
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{"container": observer, "network": network, "label": observer}
	pending := map[string]bool{}
	save := func() {
		t.Helper()
		body, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(recovery, "resources.next.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(recovery, "resources.next.json"), filepath.Join(recovery, "resources.json")); err != nil {
			t.Fatal(err)
		}
	}
	// This directory contains no secrets. Unlike t.TempDir mounts, its
	// ownership record must survive an incomplete cleanup or process exit.
	t.Cleanup(func() {
		if len(pending) != 0 {
			t.Errorf("resources may remain; recovery manifest: %s", filepath.Join(recovery, "resources.json"))
			return
		}
		if err := os.RemoveAll(recovery); err != nil {
			t.Error(err)
		}
	})
	save()
	// A killed run skips every cleanup, so name the manifest before anything
	// exists.
	t.Logf("recovery manifest until cleanup: %s", filepath.Join(recovery, "resources.json"))
	ctx, cancel := context.WithTimeout(t.Context(), codexUsageInvocationDeadline+90*time.Second)
	defer cancel()
	labels := []Label{{Key: "freeside.usage-probe", Value: observer}}
	pending[network] = true
	if err := rt.CreateNetwork(ctx, network, labels); err != nil {
		t.Fatal(err)
	}
	netBefore, err := rt.InspectNetwork(ctx, network)
	if err != nil || !netBefore.LabelsObserved || !slices.Equal(netBefore.Labels, labels) || netBefore.CreationDate == "" {
		t.Fatal("network ownership unproved; leave for recovery")
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		now, err := rt.InspectNetwork(cleanup, network)
		if err != nil || !now.LabelsObserved || !slices.Equal(now.Labels, labels) || now.CreationDate != netBefore.CreationDate {
			t.Error("network ownership changed; leave for recovery")
			return
		}
		if err := rt.DeleteNetwork(cleanup, network); err != nil {
			t.Error(err)
			return
		}
		delete(pending, network)
	})
	manifest["network_created"] = netBefore.CreationDate
	save()
	if netBefore.Mode != NetworkHostOnly {
		t.Fatal("probe needs a host-only network")
	}
	create := func(spec ContainerSpec) {
		t.Helper()
		name := spec.Name
		expectedNetworks := []string{}
		if !spec.NetworkDisabled {
			expectedNetworks = append(expectedNetworks, spec.Network)
		}
		manifest[name] = "creation pending"
		save()
		pending[name] = true
		if err := rt.CreateContainer(ctx, spec); err != nil {
			t.Fatal(err)
		}
		before, err := rt.Inspect(ctx, name)
		if err != nil || !before.LabelsObserved || !slices.Equal(before.Labels, labels) || before.CreationDate == "" {
			t.Fatal("container ownership unproved; leave for recovery")
		}
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), time.Minute)
			defer done()
			now, err := rt.Inspect(cleanup, name)
			if err != nil || !now.LabelsObserved || !slices.Equal(now.Labels, labels) || now.CreationDate != before.CreationDate {
				t.Error("container ownership changed; leave for recovery")
				return
			}
			if now.State != StateStopped {
				if err := rt.StopContainer(cleanup, name); err != nil {
					t.Error(err)
					return
				}
			}
			if err := rt.DeleteContainer(cleanup, name); err != nil {
				t.Error(err)
				return
			}
			delete(pending, name)
		})
		manifest[name] = before.CreationDate
		save()
		if !before.AllowlistFieldsObserved || !sameImage(image, before.ImageReference) || !slices.Equal(before.Command, spec.Command) || !before.NetworksObserved || before.NetworkAttachmentCount != len(expectedNetworks) ||
			!slices.Equal(before.Networks, expectedNetworks) || before.SSH || len(before.PublishedPorts) != 0 || len(before.PublishedSockets) != 0 || !sameMounts(before.Mounts, spec.Mounts) {
			t.Fatalf("probe topology differs: fields=%t image=%t command=%t networks=%t count=%t names=%t forwarding=%t mounts=%t",
				before.AllowlistFieldsObserved, sameImage(image, before.ImageReference), slices.Equal(before.Command, spec.Command),
				before.NetworksObserved, before.NetworkAttachmentCount == len(expectedNetworks), slices.Equal(before.Networks, expectedNetworks),
				!before.SSH && len(before.PublishedPorts) == 0 && len(before.PublishedSockets) == 0, sameMounts(before.Mounts, spec.Mounts))
		}
	}

	proxy := newCodexUsageProxy(t, mode == "turn", netBefore.IPv4Subnet, upstream)
	_, port, err := net.SplitHostPort(proxy.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	input, auth := t.TempDir(), t.TempDir()
	// The snapshot can hold a real access token until the seeder has copied it.
	if err := os.Chmod(auth, 0o700); err != nil { //nolint:gosec // owner-only test directory
		t.Fatal(err)
	}
	// A killed run skips t.TempDir cleanup too, so the manifest names this
	// copy before it exists and until it is removed.
	manifest["host_snapshot_copy"] = filepath.Join(auth, "auth.json")
	save()
	if err := os.WriteFile(filepath.Join(auth, "auth.json"), body, 0o600); err != nil {
		t.Fatal("write private access-only input")
	}
	settings, err := json.Marshal(map[string]any{
		"proxy": "http://" + net.JoinHostPort(netBefore.IPv4Gateway, port),
		"mode":  mode, "deadline": int(codexUsageInvocationDeadline / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	for target, source := range map[string]string{"probe.sh": "testdata/codex_usage_probe.sh", "sanitize.jq": "testdata/codex_usage_sanitize.jq"} {
		fixture, err := os.ReadFile(source) //nolint:gosec // source is one of two literal repository fixture paths
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(input, target), fixture, 0o600); err != nil { //nolint:gosec // literal fixture basenames under a private t.TempDir
			t.Fatal(err)
		}
	}
	for target, content := range map[string][]byte{"config.json": settings, "ca.pem": proxy.ca} {
		if err := os.WriteFile(filepath.Join(input, target), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	volume := prefix + "-snapshot"
	manifest[volume] = "creation pending"
	save()
	pending[volume] = true
	if err := rt.CreateVolume(ctx, volume, 16, labels); err != nil {
		t.Fatal(err)
	}
	volumeBefore, err := rt.InspectVolume(ctx, volume)
	if err != nil || !volumeBefore.LabelsObserved || !slices.Equal(volumeBefore.Labels, labels) || volumeBefore.CreationDate == "" {
		t.Fatal("snapshot ownership unproved; leave for recovery")
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		now, err := rt.InspectVolume(cleanup, volume)
		if err != nil || !now.LabelsObserved || !slices.Equal(now.Labels, labels) || now.CreationDate != volumeBefore.CreationDate {
			t.Error("snapshot ownership changed; leave for recovery")
			return
		}
		if err := rt.DeleteVolume(cleanup, volume); err != nil {
			t.Error(err)
			return
		}
		delete(pending, volume)
	})
	manifest[volume] = volumeBefore.CreationDate
	save()

	seeder := prefix + "-seeder"
	create(ContainerSpec{
		Name: seeder, Image: image, NetworkDisabled: true, Labels: labels, Size: DefaultLaunchSize(LaunchConformance),
		Mounts: []Mount{{Type: MountVolume, Source: volume, Target: "/snapshot"}}, Command: []string{"sleep", "180"},
	})
	if err := rt.StartContainer(ctx, seeder); err != nil {
		t.Fatal(err)
	}
	for _, copy := range []struct{ source, target string }{{input, "/input"}, {auth, "/auth"}} {
		if err := rt.CopyIntoContainer(ctx, seeder, copy.source, copy.target); err != nil {
			t.Fatal("seed copy failed")
		}
	}
	seed := exec.CommandContext(ctx, bin, "exec", seeder, "sh", "-c", "set -eu; cp -R /input/. /snapshot/; cp /auth/auth.json /snapshot/auth.json; chmod 0400 /snapshot/auth.json; rm /auth/auth.json") //nolint:gosec // fixed command and test-owned resources
	if err := seed.Run(); err != nil {
		t.Fatal("snapshot seed failed")
	}
	// From here the only copy is the read-only volume the manifest names.
	if err := os.Remove(filepath.Join(auth, "auth.json")); err != nil {
		t.Fatal("remove private access-only input")
	}
	manifest["host_snapshot_copy"] = "removed"
	save()
	if err := rt.StopContainer(ctx, seeder); err != nil {
		t.Fatal(err)
	}
	stopped, err := rt.Inspect(ctx, seeder)
	if err != nil || stopped.State != StateStopped {
		t.Fatal("snapshot seeder did not stop")
	}
	create(ContainerSpec{
		Name: observer, Image: image, Network: network, Labels: labels, Size: DefaultLaunchSize(LaunchConformance),
		Mounts: []Mount{
			{Type: MountVolume, Source: volume, Target: "/probe", ReadOnly: true},
			{Type: MountVolume, Source: volume, Target: codexReviewSnapshotTarget, ReadOnly: true},
		},
		Command: []string{"sleep", "180"},
	})
	if err := rt.StartContainer(ctx, observer); err != nil {
		t.Fatal(err)
	}
	// Setup spent real time, so the admission that counts is this one. The
	// observer has only slept so far; anything the proxy saw contradicts the
	// deferral and the analyzer rejects it.
	if gate != nil && !gate() {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		deferred.Requests, deferred.Failures = slices.Clone(proxy.requests), proxy.failures
		return deferred
	}
	// The driver reports how long after this instant its session started.
	gated := strconv.FormatInt(time.Now().Unix(), 10)
	// The driver is synchronous, so no real-clock polling is needed. Never attach
	// its stdout/stderr to test output: error paths can contain private data.
	driver := exec.CommandContext(ctx, bin, "exec", observer, "sh", "/probe/probe.sh", gated) //nolint:gosec // fixed driver, a decimal clock reading, and a test-owned container
	if err := driver.Run(); err != nil {
		t.Fatal("probe_failed: driver failed")
	}
	read := func(path string, limit int64) []byte {
		t.Helper()
		command := exec.CommandContext(ctx, bin, "exec", observer, "cat", path) //nolint:gosec // fixed path and test-owned container
		pipe, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			t.Fatal("probe_failed: capture unavailable")
		}
		content, readErr := io.ReadAll(io.LimitReader(pipe, limit+1))
		_ = pipe.Close()
		if err := command.Wait(); err != nil || readErr != nil {
			t.Fatal("probe_failed: capture failed")
		}
		return content
	}
	if privateCapture != "" {
		// Raw app-server output, for private schema diagnosis only. It is
		// written before the reviewed capture is decoded so a rejected
		// schema can still be diagnosed.
		if err := os.WriteFile(filepath.Join(privateCapture, name+".raw.jsonl"), read("/capture/raw.jsonl", 1<<20), 0o600); err != nil { //nolint:gosec // literal case name under the operator's private directory
			t.Fatal("private capture write failed")
		}
	}
	result, err := codexUsageDecode(read("/capture/result.json", 16<<10))
	if err != nil {
		t.Fatal("probe_failed: capture schema rejected")
	}
	if err := rt.StopContainer(ctx, observer); err != nil {
		t.Fatal(err)
	}
	proxy.server.Close()
	proxy.wg.Wait()
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return codexUsageEvidence{Case: name, Gate: "admitted", Capture: &result, Requests: slices.Clone(proxy.requests), Failures: proxy.failures}
}
