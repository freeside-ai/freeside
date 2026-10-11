package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// errJudgmentCredentialUnavailable is the one reason a judgment role is
// unbound for its credential. It is fixed because the reason keys the role's
// health item and reaches the preflight manifest: the cause, which names a
// volume and a runtime step, goes to the log instead.
var errJudgmentCredentialUnavailable = errors.New(
	"the stored credential of the line's enrollment could not be read; the log names the cause, " +
		"and the next call reads it again")

// judgmentCredentialRead reads the token of an enrollment's generation
// numbered ordinal out of its credential volume, or fails with no token.
type judgmentCredentialRead func(
	ctx context.Context, enrollment domain.ClientEnrollmentID, ordinal int,
) (ward.SetupToken, error)

// judgmentCredentials hands each judgment call the token of the enrollment
// generation its agent names (issue #1426). The token is read from that
// generation's credential volume and then held in memory, so a daemon makes
// one read per enrollment generation and no call waits on a container after
// the first. Only a token is remembered: a failed read is tried again by the
// next call, so a store repaired in place binds the role without a restart.
// A newer generation's token replaces the older one's, and the older one is
// dropped before the newer is read, so a failed read leaves no replaced token
// held.
type judgmentCredentials struct {
	read   judgmentCredentialRead
	logger *slog.Logger

	// reading admits one caller at a time and guards held. It is a channel
	// and not a mutex so that a caller waiting behind a slow read still
	// leaves at its own deadline.
	reading chan struct{}
	held    map[domain.ClientEnrollmentID]heldJudgmentCredential
}

// heldJudgmentCredential's token field is exported so that fmt and slog, which
// cannot call a formatter on an unexported field, print the Secret's
// placeholder when an entry is rendered by itself. A rendered source reaches
// its entries through an unexported map, which is what Format is for.
type heldJudgmentCredential struct {
	ordinal int
	Token   inference.Secret
}

func newJudgmentCredentials(read judgmentCredentialRead, logger *slog.Logger) *judgmentCredentials {
	return &judgmentCredentials{
		read: read, logger: logger, reading: make(chan struct{}, 1),
		held: map[domain.ClientEnrollmentID]heldJudgmentCredential{},
	}
}

// Format prints one fixed word for a source, whatever the verb. Without it
// fmt and slog would walk the unexported map and print each held token in
// the clear, because they cannot call Secret's formatter there.
func (c *judgmentCredentials) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "judgmentCredentials([REDACTED])")
}

// token returns the credential for one enrollment generation. One caller
// reads at a time on purpose: every role on an enrollment asks at once at
// startup, and they share one read instead of each launching its own.
func (c *judgmentCredentials) token(
	ctx context.Context, enrollment domain.ClientEnrollmentID, ordinal int,
) (inference.Secret, error) {
	if c == nil || c.read == nil {
		return "", errJudgmentCredentialUnavailable
	}
	select {
	case c.reading <- struct{}{}:
	default:
		select {
		case c.reading <- struct{}{}:
		case <-ctx.Done():
			return "", c.unavailable(enrollment, ordinal, ctx.Err())
		}
	}
	defer func() { <-c.reading }()
	if held, ok := c.held[enrollment]; ok {
		if held.ordinal == ordinal {
			return held.Token, nil
		}
		// A request for a newer generation makes the held token a replaced
		// one whether or not the read below succeeds. A request that names an
		// older generation is stale, and must not cost the current token.
		if held.ordinal < ordinal {
			delete(c.held, enrollment)
		}
	}
	stored, err := c.read(ctx, enrollment, ordinal)
	if err != nil {
		return "", c.unavailable(enrollment, ordinal, err)
	}
	token := inference.Secret(stored.Reveal())
	if token == "" {
		return "", errJudgmentCredentialUnavailable
	}
	c.held[enrollment] = heldJudgmentCredential{ordinal: ordinal, Token: token}
	return token, nil
}

// unavailable logs why a generation's credential was not read and returns
// the fixed refusal, which carries none of it.
func (c *judgmentCredentials) unavailable(enrollment domain.ClientEnrollmentID, ordinal int, cause error) error {
	if c.logger != nil {
		c.logger.Warn("judgment credential not read; the role is unbound until a read succeeds",
			"enrollment", enrollment, "generation", ordinal, "error", cause)
	}
	return errJudgmentCredentialUnavailable
}

// productionSetupTokenReader reads a setup-token volume through the daemon's
// pinned exporter image, as the credential-integrity probe observes one.
func productionSetupTokenReader(
	runtime ward.Runtime, exporterImage string, authorize ward.RuntimeResourceAuthorizer,
) func(context.Context, string) (ward.SetupTokenRead, error) {
	return func(ctx context.Context, volume string) (ward.SetupTokenRead, error) {
		return ward.ReadSetupToken(ctx, runtime, exporterImage, volume, authorize)
	}
}

// productionJudgmentCredentials is the running daemon's source: each read
// holds the identity's shared read hold, so no refresh or enrollment changes
// the store under it.
func productionJudgmentCredentials(
	st *store.Store, cfg claudeDriverConfig, runtime ward.Runtime, logger *slog.Logger,
) *judgmentCredentials {
	reader := wardstore.CredentialReader{
		Store: st,
		ReadSetupToken: productionSetupTokenReader(
			runtime, cfg.ExporterImage, productionRigRuntimeAuthorizer(cfg.StateDir, cfg.RigTokenFile)),
		Now: func() time.Time { return time.Now().UTC() },
	}
	return newJudgmentCredentials(reader.Read, logger)
}
