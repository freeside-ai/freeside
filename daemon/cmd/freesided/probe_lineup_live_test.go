package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestLiveProbeLineup runs one real probe per shape through the production
// prober: a Claude writer on the agent image, a Codex reviewer on the review
// image, and judgment on the pinned host binary. Each call bills its account
// once; the logged usage is the per-role cost the README cites. CI skips it.
// Missing inputs in an opted-in run fail, so a skipped shape cannot pass.
func TestLiveProbeLineup(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_WARD_LIVE_TEST=1; requires Apple container, pinned images, and provider credentials; see daemon/README.md")
	}
	required := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required", name)
		}
		return value
	}
	token, err := ward.NewSetupToken([]byte(required("CLAUDE_CODE_OAUTH_TOKEN")))
	if err != nil {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN is not a setup token")
	}
	claudeModel := required("FREESIDE_PROBE_LINEUP_CLAUDE_MODEL")
	codexModel := required("FREESIDE_WARD_CODEX_REVIEW_MODEL")
	// The reviewer's snapshot is read through the production private-input
	// checks, so its directory is the input root and must be owner-only.
	reviewAuth := required("FREESIDE_WARD_CODEX_REVIEW_AUTH")
	var settings loadedDeputySettings
	settings.Review.InputRoot, settings.Review.AuthSnapshot = filepath.Dir(reviewAuth), reviewAuth
	settings.Review.AuthMode = ward.CodexAuthSubscription
	settings.Judgment.ClaudeBin = required("FREESIDE_JUDGMENT_CLI")
	settings.Judgment.ClaudeSHA256 = required("FREESIDE_JUDGMENT_CLI_SHA256")
	bin, err := exec.LookPath("container")
	if err != nil {
		t.Fatal(err)
	}
	prober := &productionLineupProber{
		settings: &settings, runtime: ward.NewCLIRuntime(bin), now: time.Now, token: &token,
	}
	for _, call := range []lineupCall{
		{
			Roles: []domain.RoleName{domain.RoleImplementer}, Surface: lineupSurfaceAgentImage,
			Harness: domain.HarnessClientClaudeCode, Image: required("FREESIDE_WARD_CLAUDE_AGENT_IMAGE"),
			Model: claudeModel, RequestedEffort: domain.EffortHigh, NativeEffort: "high",
			PinnedBuild: agentbaseline.ClaudeCodeHarnessBuild,
		},
		{
			Roles: []domain.RoleName{domain.RoleReviewer}, Surface: lineupSurfaceReviewImage,
			Harness: domain.HarnessClientCodexCLI, Image: required("FREESIDE_WARD_CODEX_AGENT_IMAGE"),
			Model: codexModel, RequestedEffort: domain.EffortHigh, NativeEffort: "high",
			PinnedBuild: agentbaseline.CodexHarnessBuild,
		},
		{
			Roles: judgmentProbeRoles(), Surface: lineupSurfaceJudgmentBinary,
			Harness: domain.HarnessClientClaudeCode, Model: claudeModel,
			RequestedEffort: domain.EffortHigh, NativeEffort: "high",
			PinnedBuild: agentbaseline.ClaudeCallHarnessBuild,
		},
	} {
		t.Run(string(call.Surface), func(t *testing.T) {
			record := probeLineup(t.Context(), []lineupCall{call}, prober, "", "live", time.Now().UTC())
			entry := record.Calls[0]
			t.Logf("%s: verdict %s, build %v, usage %s", call.Surface, entry.Verdict,
				derefOr(entry.ObservedBuild, "unknown"), formatLineupUsage(entry.Usage))
			if !record.Passed {
				t.Fatalf("%s did not pass: %s", call.Surface, derefOr(entry.Refusal, ""))
			}
		})
	}
}

func derefOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func formatLineupUsage(usage ward.LineupProbeUsage) string {
	count := func(value *int64) string {
		if value == nil {
			return "unreported"
		}
		return strconv.FormatInt(*value, 10)
	}
	return "input " + count(usage.InputTokens) + ", output " + count(usage.OutputTokens) +
		", cost_usd " + derefOr(usage.CostUSD, "unreported")
}
