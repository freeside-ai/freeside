package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/seedfixture"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// SeedStartupFixture seeds the named fixture into a store that must hold
// no runs or attention items yet.
func SeedStartupFixture(
	parent context.Context, st *store.Store, fixture seedfixture.Name, dbPath string, logger *slog.Logger,
) error {
	inventory, err := seedfixture.Seed(parent, st, fixture)
	if errors.Is(err, seedfixture.ErrStoreNotEmpty) {
		return fmt.Errorf("-seed-fixture: store %q already holds runs or attention items", dbPath)
	}
	if err != nil {
		// The fixture commits in several transactions, so a failure after
		// the first leaves a partial fixture that the emptiness check then
		// refuses to reseed.
		return fmt.Errorf("-seed-fixture %s: %w (store %q may be partially seeded; delete it before retrying)",
			fixture, err, dbPath)
	}
	if logger != nil {
		logger.Info("seeded fixture", "fixture", fixture,
			"projects", inventory.Projects, "tasks", inventory.Tasks, "runs", inventory.Runs,
			"attention_items", inventory.AttentionItems)
	}
	return nil
}

// MaintainLocalBackups runs the startup backup pass and returns the
// producer whose loop startServing starts.
func MaintainLocalBackups(
	parent context.Context, st *store.Store, blobs *signet.BlobStore, localBackupFiles *store.LocalBackupFiles,
	logger *slog.Logger,
) (*store.LocalBackupProducer, error) {
	// Backup evidence is maintained before anything can admit work. Orphan
	// reconciliation in composeClaudeWorkflow resumes writers through the
	// unattended admission gate, which reads backup health, so a pass that has not yet scanned the
	// live database would let that gate see a stale checkpoint's verdict.
	localBackups, err := localBackupFiles.NewProducer(st)
	if err != nil {
		return nil, err
	}
	// A store written before proposal paths persisted their evidence bodies
	// holds carrier rows whose bytes the closure scan cannot verify. Rebuild
	// them from the durable proposal rows first, so this pass already sees them.
	// A failed rebuild is reported, never fatal, for the reason below: it
	// re-gates every proposal row, including ones no backup pass reads, and a
	// body it could not write still surfaces as unhealthy backup closure.
	if restored, err := signet.RestoreProposalEvidence(parent, st, blobs); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
	} else if restored > 0 && logger != nil {
		logger.Info("restored effect-proposal evidence bodies", "count", restored)
	}
	if err := localBackups.Maintain(parent); err != nil {
		// A durable row this binary cannot reconstruct is reported, never
		// fatal: startup is the one pass an operator cannot retry past, and
		// the dominant cause is a downgrade that starting is what lets them
		// undo. Backup health carries the refusal (store.ErrBackupClosureIncomplete).
		if !errors.Is(err, store.ErrBackupClosureIncomplete) {
			return nil, err
		}
		fmt.Fprintln(os.Stderr, "freesided:", err)
	}
	return localBackups, nil
}
