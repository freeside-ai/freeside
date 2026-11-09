package domain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func mustComputeDigest(t *testing.T, compute func() (domain.Digest, error)) domain.Digest {
	t.Helper()
	digest, err := compute()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestTranslateEffortClaudeCode(t *testing.T) {
	want := map[domain.EffortLevel]string{
		domain.EffortHarnessDefault: "",
		domain.EffortLow:            "low",
		domain.EffortMedium:         "medium",
		domain.EffortHigh:           "high",
		domain.EffortMax:            "max",
	}
	for _, effort := range domain.AllEffortLevels {
		t.Run(string(effort), func(t *testing.T) {
			native, ok := want[effort]
			if !ok {
				t.Fatalf("no expected native value for %q: extend this table with the translation", effort)
			}
			got, err := domain.TranslateEffort(domain.HarnessClientClaudeCode, effort)
			if err != nil {
				t.Fatal(err)
			}
			if got.Requested != effort || got.Native != native {
				t.Fatalf("TranslateEffort(%q) = %+v, want native %q", effort, got, native)
			}
		})
	}
	if got := domain.ClaudeCodeSendableEfforts(); !slices.Equal(got, domain.AllEffortLevels) {
		t.Fatalf("ClaudeCodeSendableEfforts() = %v, want every level", got)
	}
}

// Codex has no max: max is sent as xhigh, its highest level, and the
// translation is a clamp.
func TestTranslateEffortCodexCLI(t *testing.T) {
	want := map[domain.EffortLevel]string{
		domain.EffortHarnessDefault: "",
		domain.EffortLow:            "low",
		domain.EffortMedium:         "medium",
		domain.EffortHigh:           "high",
		domain.EffortMax:            "xhigh",
	}
	for _, effort := range domain.AllEffortLevels {
		t.Run(string(effort), func(t *testing.T) {
			native, ok := want[effort]
			if !ok {
				t.Fatalf("no expected native value for %q: extend this table with the translation", effort)
			}
			got, err := domain.TranslateEffort(domain.HarnessClientCodexCLI, effort)
			if err != nil {
				t.Fatal(err)
			}
			if got.Requested != effort || got.Native != native {
				t.Fatalf("TranslateEffort(%q) = %+v, want native %q", effort, got, native)
			}
			if clamped := effort == domain.EffortMax; got.Clamped() != clamped {
				t.Fatalf("TranslateEffort(%q) clamped = %t, want %t", effort, got.Clamped(), clamped)
			}
		})
	}
	max, err := domain.TranslateEffort(domain.HarnessClientCodexCLI, domain.EffortMax)
	if err != nil || max.String() != "max → xhigh" {
		t.Fatalf("max renders as %q, %v", max.String(), err)
	}
	if got := domain.CodexCLISendableEfforts(); !slices.Equal(got, domain.AllEffortLevels) {
		t.Fatalf("CodexCLISendableEfforts() = %v, want every level", got)
	}
}

func TestTranslateEffortRefusals(t *testing.T) {
	cases := []struct {
		name    string
		client  domain.HarnessClientKind
		effort  domain.EffortLevel
		wantErr error
	}{
		{"codex level outside the table", domain.HarnessClientCodexCLI, "minimal", domain.ErrEffortUntranslatable},
		{"claude level outside the table", domain.HarnessClientClaudeCode, "xhigh", domain.ErrEffortUntranslatable},
		{"unknown client", "pi", domain.EffortHigh, domain.ErrInvalidHarnessClientKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := domain.TranslateEffort(tc.client, tc.effort); !errors.Is(err, tc.wantErr) {
				t.Fatalf("TranslateEffort = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestEffortTranslationRendering(t *testing.T) {
	cases := []struct {
		name          string
		translation   domain.EffortTranslation
		wantString    string
		wantEffective string
		wantClamped   bool
	}{
		{"clamp", domain.EffortTranslation{Requested: domain.EffortMax, Native: "xhigh"}, "max → xhigh", "xhigh", true},
		{"unclamped", domain.EffortTranslation{Requested: domain.EffortHigh, Native: "high"}, "high", "high", false},
		{
			"harness default sends nothing",
			domain.EffortTranslation{Requested: domain.EffortHarnessDefault},
			"harness_default", "harness_default", false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := tc.translation
			if tr.String() != tc.wantString || tr.Effective() != tc.wantEffective || tr.Clamped() != tc.wantClamped {
				t.Fatalf("rendering = %q/%q/%v, want %q/%q/%v", tr.String(), tr.Effective(), tr.Clamped(),
					tc.wantString, tc.wantEffective, tc.wantClamped)
			}
		})
	}
}

func TestClaudeCodeLaunchModel(t *testing.T) {
	if got := domain.ClaudeCodeLaunchModel(domain.ClaudeCodeNativeDefaultRouteModelID); got != "" {
		t.Fatalf("native default passes model %q, want none", got)
	}
	if got := domain.ClaudeCodeLaunchModel("claude-opus-5-5"); got != "claude-opus-5-5" {
		t.Fatalf("explicit model passes %q, want claude-opus-5-5", got)
	}
}
