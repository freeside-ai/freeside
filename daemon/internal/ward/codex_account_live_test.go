package ward

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func codexAccountSynthetic(t *testing.T) []byte {
	t.Helper()
	claims := map[string]any{
		"email": "probe@example.invalid", "exp": time.Now().Add(2 * time.Minute).Unix(),
		"https://api.openai.com/auth": map[string]string{"chatgpt_plan_type": "plus", "chatgpt_account_id": "synthetic-account"},
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".synthetic"
	body, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": nil,
		"tokens":       map[string]string{"id_token": token, "access_token": token, "refresh_token": "", "account_id": "synthetic-account"},
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspectCodexAuthSnapshot(CodexAuthSubscription, body); err != nil {
		t.Fatal("synthetic store violates snapshot contract")
	}
	return body
}

// Select /near_expiry or /control to exercise synthetic cases without an
// operator credential. The complete suite still fails when its real input is absent.
func TestLiveCodexAccountProbe(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_WARD_LIVE_TEST=1; see Codex Account Probe Spike in daemon/README.md")
	}
	image := os.Getenv("FREESIDE_WARD_CODEX_AGENT_IMAGE")
	if !digestPinnedImagePattern.MatchString(image) {
		t.Fatal("FREESIDE_WARD_CODEX_AGENT_IMAGE must be digest pinned")
	}
	bin, err := exec.LookPath("container")
	if err != nil {
		t.Fatal(err)
	}
	rt := NewCLIRuntime(bin)
	verdicts := map[string]string{}
	for _, mode := range []string{"real", "near_expiry", "control"} {
		t.Run(mode, func(t *testing.T) {
			body := codexAccountSynthetic(t)
			authPath := ""
			if mode == "real" {
				authPath = os.Getenv("FREESIDE_WARD_CODEX_REVIEW_AUTH")
				if authPath == "" {
					t.Fatal("probe_failed: FREESIDE_WARD_CODEX_REVIEW_AUTH is required for real case")
				}
				var err error
				body, err = os.ReadFile(authPath) //nolint:gosec // explicit operator-selected access-only file; never logged
				if err != nil {
					t.Fatal("probe_failed: access-only input unavailable")
				}
				expires, err := inspectCodexAuthSnapshot(CodexAuthSubscription, body)
				if err != nil || expires == nil || time.Until(*expires) <= 10*time.Minute {
					t.Fatal("probe_failed: input must be access-only with more than ten minutes remaining")
				}
			}
			original := sha256.Sum256(body)
			evidence := codexAccountRun(t, rt, bin, image, mode, body)
			if authPath != "" {
				after, err := os.ReadFile(authPath) //nolint:gosec // same operator-selected file, hash only
				if err != nil {
					t.Fatal("probe_failed: cannot verify operator input after run")
				}
				if sha256.Sum256(after) != original {
					evidence.Capture.AuthUnchanged = false
				}
			}
			evidence.Verdict = codexAccountAnalyze(evidence.Capture, mode == "control", evidence.Requests, evidence.Failures)
			verdicts[mode] = evidence.Verdict
			encoded, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(string(encoded))
			if evidence.Verdict != "pass" {
				t.Errorf("%s: %s case", evidence.Verdict, mode)
			}
		})
	}
	overall := "probe_failed"
	if len(verdicts) == 3 && verdicts["control"] == "pass" {
		overall = "pass"
		for _, mode := range []string{"real", "near_expiry"} {
			if verdicts[mode] == "probe_failed" {
				overall = "probe_failed"
				break
			}
			if verdicts[mode] == "fail" {
				overall = "fail"
			}
		}
	}
	t.Logf("overall=%s (requires all three cases and a proven refresh detector)", overall)
}

func codexAccountRun(t *testing.T, rt *CLIRuntime, bin, image, mode string, body []byte) codexAccountEvidence {
	t.Helper()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	prefix := "freeside-account-" + hex.EncodeToString(random[:])
	name, network := prefix+"-observer", prefix+"-egress"
	recovery, err := os.MkdirTemp("", "freeside-account-recovery-")
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{"container": name, "network": network, "label": name}
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
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	labels := []Label{{Key: "freeside.account-probe", Value: name}}
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
	create := func(spec ContainerSpec) InspectReport {
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
		return before
	}

	proxy := newCodexAccountProxy(t, netBefore.IPv4Subnet)
	_, port, err := net.SplitHostPort(proxy.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	input, auth := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(auth, "auth.json"), body, 0o600); err != nil { //nolint:gosec // fixed basename in private t.TempDir; credential bytes affect contents, never the path
		t.Fatal("write private access-only input")
	}
	settings, err := json.Marshal(map[string]any{
		"proxy":    "http://" + net.JoinHostPort(netBefore.IPv4Gateway, port),
		"requests": []any{map[string]any{"method": "account/read", "params": map[string]bool{"refreshToken": mode == "control"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for target, source := range map[string]string{"probe.sh": "testdata/codex_account_probe.sh", "sanitize.jq": "testdata/codex_account_sanitize.jq"} {
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
	seed := exec.CommandContext(ctx, bin, "exec", seeder, "sh", "-c", "set -eu; cp -R /input/. /snapshot/; cp /auth/auth.json /snapshot/auth.json; chmod 0400 /snapshot/auth.json") //nolint:gosec // fixed command and test-owned resources
	if err := seed.Run(); err != nil {
		t.Fatal("snapshot seed failed")
	}
	if err := rt.StopContainer(ctx, seeder); err != nil {
		t.Fatal(err)
	}
	stopped, err := rt.Inspect(ctx, seeder)
	if err != nil || stopped.State != StateStopped {
		t.Fatal("snapshot seeder did not stop")
	}
	create(ContainerSpec{
		Name: name, Image: image, Network: network, Labels: labels, Size: DefaultLaunchSize(LaunchConformance),
		Mounts: []Mount{
			{Type: MountVolume, Source: volume, Target: "/probe", ReadOnly: true},
			{Type: MountVolume, Source: volume, Target: codexReviewSnapshotTarget, ReadOnly: true},
		},
		Command: []string{"sleep", "180"},
	})
	if err := rt.StartContainer(ctx, name); err != nil {
		t.Fatal(err)
	}
	// The driver is synchronous, so no real-clock polling is needed. Never attach
	// its stdout/stderr to test output: error paths can contain private data.
	driver := exec.CommandContext(ctx, bin, "exec", name, "sh", "/probe/probe.sh") //nolint:gosec // fixed driver and test-owned container
	if err := driver.Run(); err != nil {
		t.Fatal("probe_failed: driver failed")
	}
	command := exec.CommandContext(ctx, bin, "exec", name, "cat", "/capture/result.json") //nolint:gosec // fixed path and test-owned container
	pipe, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal("probe_failed: capture unavailable")
	}
	captureBody, readErr := io.ReadAll(io.LimitReader(pipe, (16<<10)+1))
	_ = pipe.Close()
	if err := command.Wait(); err != nil || readErr != nil {
		t.Fatal("probe_failed: capture failed")
	}
	result, err := codexAccountDecode(captureBody)
	if err != nil {
		t.Fatal("probe_failed: capture schema rejected")
	}
	if err := rt.StopContainer(ctx, name); err != nil {
		t.Fatal(err)
	}
	proxy.server.Close()
	proxy.wg.Wait()
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return codexAccountEvidence{Case: mode, Capture: result, Requests: slices.Clone(proxy.requests), Failures: proxy.failures}
}
