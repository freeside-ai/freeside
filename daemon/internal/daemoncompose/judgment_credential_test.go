package daemoncompose

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	judgmentSentinelToken = "sk-ant-oat-JUDGMENT-SENTINEL-0123456789abcdefghijklmnopq" //nolint:gosec // G101: test fixture
	judgmentSecondToken   = "sk-ant-oat-JUDGMENT-SECOND-0123456789abcdefghijklmnopqrs" //nolint:gosec // G101: test fixture
)

func judgmentTestToken(t *testing.T, value string) ward.SetupToken {
	t.Helper()
	token, err := ward.NewSetupToken([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// The credential source remembers only a token: a nil source and a read that
// hands back nothing are both the fixed refusal.
func TestJudgmentCredentialsFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const enrollment = domain.ClientEnrollmentID("claude-main/claude_code")
	var none *JudgmentCredentials
	if token, err := none.Token(ctx, enrollment, 1); !errors.Is(err, ErrJudgmentCredentialUnavailable) || token != "" {
		t.Fatalf("nil source = %v", err)
	}
	empty := NewJudgmentCredentials(func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
		return ward.SetupToken{}, nil
	}, nil)
	if token, err := empty.Token(ctx, enrollment, 1); !errors.Is(err, ErrJudgmentCredentialUnavailable) || token != "" {
		t.Fatalf("a read with no token = %v", err)
	}

	// A held token answers for its own generation only: when the read of a
	// newer one fails, the older token is not the fallback and is no longer
	// held. A stale request for an older generation leaves the current token
	// held.
	failing := false
	source := NewJudgmentCredentials(func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
		if failing {
			return ward.SetupToken{}, errors.New("volume gone")
		}
		return judgmentTestToken(t, judgmentSentinelToken), nil
	}, nil)
	if token, err := source.Token(ctx, enrollment, 1); err != nil || token != judgmentSentinelToken {
		t.Fatalf("first generation = %v", err)
	}
	failing = true
	if token, err := source.Token(ctx, enrollment, 2); !errors.Is(err, ErrJudgmentCredentialUnavailable) || token != "" {
		t.Fatalf("a failed read of a newer generation returned a token or %v", err)
	}
	if len(source.held) != 0 {
		t.Fatal("a failed read of a newer generation left the replaced token held")
	}
	failing = false
	if token, err := source.Token(ctx, enrollment, 2); err != nil || token != judgmentSentinelToken {
		t.Fatalf("second generation = %v", err)
	}
	failing = true
	if token, err := source.Token(ctx, enrollment, 1); !errors.Is(err, ErrJudgmentCredentialUnavailable) || token != "" {
		t.Fatalf("a stale request for an older generation returned a token or %v", err)
	}
	if held, ok := source.held[enrollment]; !ok || held.Ordinal != 2 {
		t.Fatal("a stale request for an older generation cost the current token")
	}
}

// Callers that miss at once share one read, and a caller waiting behind a
// read that does not end leaves at its own deadline with no token.
func TestJudgmentCredentialsShareOneReadAndWaitNoLongerThanTheirDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		const enrollment = domain.ClientEnrollmentID("claude-main/claude_code")
		release := make(chan struct{})
		reads := 0
		source := NewJudgmentCredentials(func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
			reads++
			<-release
			return judgmentTestToken(t, judgmentSentinelToken), nil
		}, nil)
		tokens := make(chan inference.Secret, 2)
		for range cap(tokens) {
			go func() {
				token, err := source.Token(ctx, enrollment, 1)
				if err != nil {
					t.Error(err)
				}
				tokens <- token
			}()
		}
		synctest.Wait()

		waiting, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		token, err := source.Token(waiting, "claude-second/claude_code", 1)
		if !errors.Is(err, ErrJudgmentCredentialUnavailable) || token != "" {
			t.Fatalf("a caller past its deadline got a token or %v", err)
		}

		close(release)
		for range cap(tokens) {
			if token := <-tokens; token != judgmentSentinelToken {
				t.Fatal("a caller that shared the read got another token")
			}
		}
		if reads != 1 {
			t.Fatalf("callers that missed at once made %d reads, want 1", reads)
		}
	})
}
