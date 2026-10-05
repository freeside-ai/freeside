package ward

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// CI deliberately skips the experiment. Missing inputs in an opted-in run
// fail, so a skipped or broken run cannot become an unsupported verdict.
func TestLiveClaudeUsage(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_WARD_LIVE_TEST=1; requires pinned Claude image and setup token; see daemon/README.md")
	}
	image := os.Getenv("FREESIDE_WARD_CLAUDE_AGENT_IMAGE")
	if !strings.Contains(image, "@sha256:") {
		t.Fatal("FREESIDE_WARD_CLAUDE_AGENT_IMAGE must be digest pinned")
	}
	token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if token == "" {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN is required")
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
	for _, mode := range []string{"idle", "turn"} {
		t.Run(mode, func(t *testing.T) {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				t.Fatal(err)
			}
			prefix := "freeside-usage-" + hex.EncodeToString(random[:])
			name, network := prefix+"-observer", prefix+"-egress"
			recovery, err := os.MkdirTemp("", "freeside-usage-recovery-")
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
			labels := []Label{{Key: "freeside.usage-probe", Value: name}}
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
			proxy := newClaudeUsageProxy(t, mode, netBefore.IPv4Subnet, nil)
			_, port, err := net.SplitHostPort(proxy.server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			input, auth, config, ready := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			for _, directory := range []string{input, config} {
				if err := os.Chmod(directory, 0o755); err != nil { //nolint:gosec // non-secret read-only mount content must be accessible to the dropped UID
					t.Fatal(err)
				}
			}
			// Each secret remains in a private input directory, mounted read-only.
			if err := os.WriteFile(filepath.Join(auth, "token"), []byte(token), 0o400); err != nil { //nolint:gosec // fixed basename in private t.TempDir; token taints contents, never the path
				t.Fatal("write private token input")
			}
			if err := os.WriteFile(filepath.Join(config, ".credentials.json"), []byte("{}"), 0o444); err != nil { //nolint:gosec // empty, non-secret auth-store sentinel on a read-only mount
				t.Fatal(err)
			}
			fixture, err := os.ReadFile("testdata/claude_usage_probe.cjs")
			if err != nil {
				t.Fatal(err)
			}
			settings, err := json.Marshal(map[string]string{"mode": mode, "proxy": "http://" + net.JoinHostPort(netBefore.IPv4Gateway, port)})
			if err != nil {
				t.Fatal(err)
			}
			for path, body := range map[string][]byte{"probe.cjs": fixture, "config.json": settings, "ca.pem": proxy.ca, "instructions.txt": []byte("Do not use tools. Reply with OK.")} {
				if err := os.WriteFile(filepath.Join(input, path), body, 0o644); err != nil { //nolint:gosec // fixed non-secret fixtures readable by the dropped UID
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
				Name: seeder, Image: image, NetworkDisabled: true, Labels: labels,
				Size:    DefaultLaunchSize(LaunchConformance),
				Mounts:  []Mount{{Type: MountVolume, Source: volume, Target: "/snapshot"}},
				Command: []string{"sh", "-c", "set -eu; n=0; while [ ! -f /ready/ready ]; do n=$((n+1)); [ \"$n\" -lt 90 ]; sleep 1; done; cp -R /input/. /snapshot/; cp /auth/token /snapshot/token; chmod 0400 /snapshot/token; touch /seeded; sleep 90"},
			})
			if err := rt.StartContainer(ctx, seeder); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ready, "ready"), []byte("ready"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, copy := range []struct{ source, target string }{{input, "/input"}, {auth, "/auth"}, {ready, "/ready"}} {
				if err := rt.CopyIntoContainer(ctx, seeder, copy.source, copy.target); err != nil {
					t.Fatal(err)
				}
			}
			waitFile := func(container, path string) {
				t.Helper()
				for {
					if exec.CommandContext(ctx, bin, "exec", container, "test", "-f", path).Run() == nil { //nolint:gosec // fixed command, test-owned resource and marker
						return
					}
					select {
					case <-ctx.Done():
						t.Fatal("probe marker timed out")
					case <-time.After(500 * time.Millisecond):
					}
				}
			}
			waitFile(seeder, "/seeded")
			// Stop the only writable attachment before mounting the snapshot read-only.
			if err := rt.StopContainer(ctx, seeder); err != nil {
				t.Fatal(err)
			}
			mounts := []Mount{{Type: MountVolume, Source: volume, Target: "/probe", ReadOnly: true}}
			spec := ContainerSpec{
				Name: name, Image: image, Network: network, Labels: labels, Mounts: mounts,
				Size: DefaultLaunchSize(LaunchConformance), Command: []string{"sh", "-c", "node /probe/probe.cjs; touch /completed; sleep 90"},
			}
			create(spec)
			if err := rt.StartContainer(ctx, name); err != nil {
				t.Fatal(err)
			}
			waitFile(name, "/completed")
			// Capture through a private pipe, never container logs or tool output.
			command := exec.CommandContext(ctx, bin, "exec", name, "cat", "/capture/result.json") //nolint:gosec // fixed command and test-owned container
			pipe, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal("capture unavailable")
			}
			body, readErr := io.ReadAll(io.LimitReader(pipe, 2<<20))
			_ = pipe.Close()
			if err := command.Wait(); err != nil || readErr != nil {
				t.Fatal("capture failed")
			}
			if privateCapture != "" {
				if err := os.WriteFile(filepath.Join(privateCapture, mode+".json"), body, 0o600); err != nil { //nolint:gosec // operator-selected empty owner-only directory; mode is a fixed test case, never provider input
					t.Fatal("private capture write failed")
				}
			}
			var result claudeUsageCapture
			if err := json.NewDecoder(bytes.NewReader(body)).Decode(&result); err != nil {
				t.Fatal("invalid private capture")
			}
			if !claudeUsageAuthUnchanged(auth, config, token) {
				t.Fatal("auth input or store changed")
			}
			// The CLI and descendants have exited; include late body/relay errors
			// before taking the verdict snapshot. Every handler has a deadline.
			proxy.server.Close()
			proxy.wg.Wait()
			proxy.mu.Lock()
			evidence := claudeUsageAnalyze(result, proxy.inference, proxy.refresh, proxy.failures)
			t.Logf("capture flags: version=%t auth_unchanged=%t malformed=%t overflow=%t spawn_failed=%t signal=%t driver_failure=%t", result.Version == "2.1.220", result.AuthUnchanged, result.Malformed, result.Overflow, result.SpawnFailed, result.Signal != nil, result.Failure != "")
			t.Logf("sanitized request trace: %v", proxy.requests)
			safe, err := json.Marshal(struct {
				Image     string              `json:"image"`
				Mode      string              `json:"mode"`
				Code      *int                `json:"exit_code"`
				Timeout   bool                `json:"timeout"`
				Inference int                 `json:"inference_requests"`
				Refresh   int                 `json:"refresh_attempts"`
				Failures  int                 `json:"transport_failures"`
				Evidence  claudeUsageEvidence `json:"evidence"`
			}{image, mode, result.Code, result.TimedOut, proxy.inference, proxy.refresh, proxy.failures, evidence})
			proxy.mu.Unlock()
			if err != nil {
				t.Fatal("encode sanitized evidence")
			}
			t.Log(string(safe))
			if evidence.Verdict == "probe_failed" {
				t.Fatal("probe failed; this is not an unsupported verdict")
			}
		})
	}
}
