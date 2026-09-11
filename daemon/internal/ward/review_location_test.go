package ward

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func TestProductionReviewLocationContract(t *testing.T) {
	for _, provider := range []reviewProvider{codexReviewProvider{}, claudeReviewProvider{}} {
		t.Run(provider.providerLabel(), func(t *testing.T) {
			prompt := provider.reviewPrompt(exec.ReviewRequest{
				BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
				Verification: testReviewVerificationEvidence(),
			})
			for _, want := range []string{
				"canonical repository-relative path in the exact reviewed diff",
				"candidate-side line numbers",
				"overlap at least one changed line",
				"anchor the finding on the causal changed line",
				"Do not pad the range",
				"pure-deletion hunk with no relevant candidate-side changed line",
				"identify the removed code and its causal failure precisely",
				"candidate-deleted file",
				"wholly file-level finding on a changed file",
				"When a relevant candidate-side changed line exists, use a concrete range",
				"never substitute whole_file for a concrete location",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("production prompt omits location rule %q", want)
				}
			}
		})
	}
	var schema struct {
		Properties struct {
			Findings struct {
				Items struct {
					Properties struct {
						Location struct {
							Description string `json:"description"`
							AnyOf       []struct {
								Properties struct {
									WholeFile struct {
										Description string `json:"description"`
									} `json:"whole_file"`
								} `json:"properties"`
							} `json:"anyOf"`
						} `json:"location"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"findings"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(reviewFindingsJSONSchema), &schema); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"candidate-side", "overlap", "changed line", "reviewed diff"} {
		if !strings.Contains(schema.Properties.Findings.Items.Properties.Location.Description, want) {
			t.Errorf("structured output schema omits location rule %q", want)
		}
	}
	location := schema.Properties.Findings.Items.Properties.Location
	if len(location.AnyOf) != 2 {
		t.Fatalf("location variants = %d, want 2", len(location.AnyOf))
	}
	for _, want := range []string{
		"pure-deletion hunk with no relevant candidate-side changed line",
		"identify the removed code and its causal failure precisely",
		"When a relevant candidate-side changed line exists, use a concrete range",
	} {
		if !strings.Contains(location.AnyOf[1].Properties.WholeFile.Description, want) {
			t.Errorf("structured output schema omits deletion location rule %q", want)
		}
	}
}

func TestReviewLocationProtocolChangesConfiguration(t *testing.T) {
	cfg, request := testCodexReview(t)
	for _, tc := range []struct {
		provider reviewProvider
		authMode CodexAuthMode
		previous string
		current  string
	}{
		{codexReviewProvider{}, request.AuthMode, "codex-production-review-prompt-v3", "codex-production-review-prompt-v4"},
		{claudeReviewProvider{}, CodexAuthSetupToken, "claude-production-review-prompt-v1", "claude-production-review-prompt-v2"},
	} {
		t.Run(tc.provider.providerLabel(), func(t *testing.T) {
			envelope, err := newCodexReviewConfigurationEnvelope(
				tc.provider, cfg, 64, tc.authMode, request.AuthIdentityID, "subscription:owner")
			if err != nil {
				t.Fatal(err)
			}
			if envelope.PromptProtocol != tc.current {
				t.Fatalf("prompt protocol = %q, want %q", envelope.PromptProtocol, tc.current)
			}
			current, err := digestCodexReviewConfigurationEnvelope(envelope)
			if err != nil {
				t.Fatal(err)
			}
			envelope.PromptProtocol = tc.previous
			previous, err := digestCodexReviewConfigurationEnvelope(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if current == previous {
				t.Fatal("changed prompt protocol retained the previous configuration approval")
			}
		})
	}
}
