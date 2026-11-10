package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// TestReviewCredentialReadsTheAdmittedGeneration: the routed Claude source
// reads another identity's token from the enrollment generation the request
// was admitted under, refuses a read the store refuses, and never prints the
// token. A Codex source reads no other identity's credential.
func TestReviewCredentialReadsTheAdmittedGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admission := fake.ReviewAdmission(domain.RoleReviewer, "run-1", "review-1")
	admission.AgentBinding.EnrollmentID, admission.AgentBinding.EnrollmentGeneration = "claude-second/claude_code", 3
	token := judgmentTestToken(t, judgmentSecondToken)
	var reads []string
	read := func(_ context.Context, id domain.ClientEnrollmentID, ordinal int) (ward.SetupToken, error) {
		reads = append(reads, fmt.Sprintf("%s@%d", id, ordinal))
		return token, nil
	}
	claude := mustRoutedReviewProvider(domain.HarnessClientClaudeCode)

	credential, err := reviewCredential(claude, read)(ctx, admission)
	if err != nil {
		t.Fatalf("read the admitted generation: %v", err)
	}
	if !slices.Equal(reads, []string{"claude-second/claude_code@3"}) {
		t.Fatalf("reads = %v, want the admitted generation", reads)
	}
	if printed := fmt.Sprintf("%v %+v %#v %s", credential, credential, credential, credential); strings.Contains(printed, judgmentSecondToken) {
		t.Fatalf("a formatted credential prints the token: %s", printed)
	}

	// A store that is not the admitted one refuses the review; a store that
	// is changing, or a read that did not finish, is retried.
	for cause, want := range map[error]domain.ReviewFailureClass{
		wardstore.ErrCredentialReadDigestMismatch:    domain.ReviewFailureConfiguration,
		wardstore.ErrCredentialReadGenerationChanged: domain.ReviewFailureConfiguration,
		wardstore.ErrCredentialReadUnsupported:       domain.ReviewFailureConfiguration,
		wardstore.ErrCredentialReadMutationLeaseLive: domain.ReviewFailureTransient,
		wardstore.ErrCredentialReadHoldEnded:         domain.ReviewFailureTransient,
		errors.New("exporter container exited"):      domain.ReviewFailureTransient,
	} {
		refusing := func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
			return ward.SetupToken{}, cause
		}
		_, err := reviewCredential(claude, refusing)(ctx, admission)
		if !errors.Is(err, cause) {
			t.Fatalf("a failed read = %v, want it to wrap %v", err, cause)
		}
		var failure *exec.ReviewSourceFailure
		switch want {
		case domain.ReviewFailureConfiguration:
			if !errors.Is(err, engine.ErrAgentNotAdmissible) || errors.As(err, &failure) {
				t.Errorf("read failing with %v = %v, want a refusal", cause, err)
			}
		default:
			if exec.ClassifyReviewSourceFailure(err) != want || errors.Is(err, engine.ErrAgentNotAdmissible) {
				t.Errorf("read failing with %v = %v, want a %s failure", cause, err, want)
			}
		}
	}

	reads = nil
	codex := mustRoutedReviewProvider(domain.HarnessClientCodexCLI)
	if _, err := reviewCredential(codex, read)(ctx, admission); !errors.Is(err, engine.ErrAgentNotAdmissible) || reads != nil {
		t.Fatalf("a Codex source = %v after reads %v, want a refusal that reads nothing", err, reads)
	}
}
