package ward

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The two ways an enrollment's provider probe refuses a setup token. Both
// refuse the enrollment; they differ in what the operator does next.
var (
	// ErrSetupTokenRejected is the provider refusing the token's
	// authentication: the operator mints a new one.
	ErrSetupTokenRejected = errors.New("the provider rejected the setup token")
	// ErrClaudeProviderUnavailable is every other failed call: the probe
	// never reached the provider, or the provider answered with an error that
	// is not about the token (overload, a rate or usage limit, no result).
	// The token is unproven either way, so the operator reruns.
	ErrClaudeProviderUnavailable = errors.New("the provider could not confirm the setup token")
)

// claudeAuthRefusalPhrases mark a refusal as the provider rejecting the
// token, matched case-insensitively against the CLI's refusal text. They
// are the one place the CLI's auth-refusal wording is encoded; the live
// probe test pins it against the real CLI.
var claudeAuthRefusalPhrases = []string{
	"api error: 401", "api error: 403", "authentication_error", "permission_error",
	"invalid api key", "invalid bearer token", "oauth token", "/login",
}

// claudeProbeRefusalWithheld replaces a refusal that echoes the token.
const claudeProbeRefusalWithheld = "[refusal withheld: it carries the setup token]"

// claudeProbeTokenWindow is the shortest run of the token's bytes a refusal
// may not carry, so a refusal cut through the token by the judge's length
// cap is still withheld.
const claudeProbeTokenWindow = 12

// classifyEnrollmentProbe judges an enrollment's provider probe. It returns
// nil when the call passed, ErrSetupTokenRejected when the provider refused
// the token's authentication, and ErrClaudeProviderUnavailable otherwise. A
// rejection needs the provider to have answered: a call that never reached it
// cannot have been refused by it, whatever the CLI printed. The error quotes
// the CLI's refusal only when it carries none of the token.
func classifyEnrollmentProbe(out LineupProbeOutput, token SetupToken) error {
	verdict := JudgeLineupProbe(domain.HarnessClientClaudeCode, "", out)
	if verdict.Passed {
		return nil
	}
	refusal := verdict.Refusal
	if refusalCarriesToken(refusal, token) {
		refusal = claudeProbeRefusalWithheld
	}
	switch {
	case !out.ProviderReached:
		return fmt.Errorf("%w: the probe never reached the provider (%s); rerun auth add once the network is back",
			ErrClaudeProviderUnavailable, refusal)
	case claudeAuthRefusal(verdict.Refusal):
		return fmt.Errorf("%w: %s; mint a new token with claude setup-token and rerun auth add",
			ErrSetupTokenRejected, refusal)
	default:
		return fmt.Errorf("%w: the provider answered with an error (%s); rerun auth add once it recovers",
			ErrClaudeProviderUnavailable, refusal)
	}
}

func claudeAuthRefusal(refusal string) bool {
	lower := strings.ToLower(refusal)
	for _, phrase := range claudeAuthRefusalPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// refusalCarriesToken reports whether refusal holds any claudeProbeTokenWindow
// bytes of the token past its public prefix.
func refusalCarriesToken(refusal string, token SetupToken) bool {
	secret := bytes.TrimPrefix(token.value, []byte(setupTokenPrefix))
	text := []byte(refusal)
	for i := 0; i+claudeProbeTokenWindow <= len(secret); i++ {
		if bytes.Contains(text, secret[i:i+claudeProbeTokenWindow]) {
			return true
		}
	}
	return false
}
