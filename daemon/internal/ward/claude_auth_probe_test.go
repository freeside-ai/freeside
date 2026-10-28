package ward

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// claudeResultLine is a one-line `claude -p --output-format json` result.
func claudeResultLine(isError bool, result string) []byte {
	body, err := json.Marshal(map[string]any{"type": "result", "is_error": isError, "result": result})
	if err != nil {
		panic(err)
	}
	return body
}

func TestClassifyEnrollmentProbe(t *testing.T) {
	token := lineupProbeTestToken(t)
	failed := func(result string) LineupProbeOutput {
		return LineupProbeOutput{Stdout: claudeResultLine(true, result), ExitStatus: 1, ProviderReached: true}
	}
	for _, tc := range []struct {
		name string
		out  LineupProbeOutput
		// want is the sentinel the error wraps; nil wants a pass.
		want error
		// says is text the error must carry.
		says string
	}{
		{
			name: "pass",
			out:  LineupProbeOutput{Stdout: claudeResultLine(false, "OK"), ProviderReached: true},
		},
		{
			name: "401 authentication error", out: failed(`Failed to authenticate. API Error: 401 ` +
				`{"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}}`),
			want: ErrSetupTokenRejected, says: "claude setup-token",
		},
		{
			name: "403 permission error", out: failed(`API Error: 403 {"type":"error","error":{"type":"permission_error"}}`),
			want: ErrSetupTokenRejected, says: "permission_error",
		},
		{name: "invalid api key", out: failed("Invalid API key · Please run /login"), want: ErrSetupTokenRejected},
		{name: "expired oauth token", out: failed("OAuth token has expired"), want: ErrSetupTokenRejected},
		{
			name: "overloaded", out: failed(`API Error: 529 {"type":"error","error":{"type":"overloaded_error"}}`),
			want: ErrClaudeProviderUnavailable, says: "provider answered with an error",
		},
		{
			name: "server error", out: failed(`API Error: 500 {"type":"error","error":{"type":"api_error"}}`),
			want: ErrClaudeProviderUnavailable, says: "rerun auth add",
		},
		{name: "usage limit", out: failed("Claude AI usage limit reached|1761609600"), want: ErrClaudeProviderUnavailable},
		{name: "rate limit", out: failed("API Error: 429 rate_limit_error"), want: ErrClaudeProviderUnavailable},
		{
			name: "no result envelope",
			out:  LineupProbeOutput{Stderr: []byte("Segmentation fault\n"), ExitStatus: 139, ProviderReached: true},
			want: ErrClaudeProviderUnavailable, says: "Segmentation fault",
		},
		{
			// The CLI can print an auth-shaped hint without hearing from the
			// provider; only the provider can reject the token.
			name: "provider never reached",
			out: LineupProbeOutput{
				Stdout: claudeResultLine(true, "API Error: Connection error. Please run /login"), ExitStatus: 1,
			},
			want: ErrClaudeProviderUnavailable, says: "never reached the provider",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyEnrollmentProbe(tc.out, token)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("classify = %v, want a pass", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("classify = %v, want %v", err, tc.want)
			}
			other := ErrSetupTokenRejected
			if errors.Is(tc.want, ErrSetupTokenRejected) {
				other = ErrClaudeProviderUnavailable
			}
			if errors.Is(err, other) {
				t.Fatalf("classify = %v wraps both classes", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("classify = %v, want it to say %q", err, tc.says)
			}
			assertNoSetupToken(t, err.Error())
		})
	}
}

// TestClassifyEnrollmentProbeWithholdsARefusalCarryingTheToken proves a
// refusal that echoes the token, whole or cut by the judge's length cap, is
// replaced by a fixed placeholder and still classified.
func TestClassifyEnrollmentProbeWithholdsARefusalCarryingTheToken(t *testing.T) {
	token := lineupProbeTestToken(t)
	cut := claudeEnrollmentTestToken[:len(claudeEnrollmentTestToken)-20]
	for name, result := range map[string]string{
		"whole token":   "API Error: 401 authentication_error for token " + claudeEnrollmentTestToken,
		"token cut off": "API Error: 401 authentication_error for token " + cut,
		"token tail":    "API Error: 401 authentication_error for ..." + claudeEnrollmentTestToken[20:],
	} {
		t.Run(name, func(t *testing.T) {
			err := classifyEnrollmentProbe(LineupProbeOutput{
				Stdout: claudeResultLine(true, result), ExitStatus: 1, ProviderReached: true,
			}, token)
			if !errors.Is(err, ErrSetupTokenRejected) {
				t.Fatalf("classify = %v, want a rejection", err)
			}
			if !strings.Contains(err.Error(), claudeProbeRefusalWithheld) || strings.Contains(err.Error(), cut[13:]) {
				t.Fatalf("classify = %v, want the refusal withheld", err)
			}
			assertNoSetupToken(t, err.Error())
		})
	}
}
