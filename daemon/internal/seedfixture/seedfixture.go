// Package seedfixture loads named development fixtures into an empty store so
// an ephemeral daemon has representative runs, tasks, and attention items to
// serve (#1503). Fixtures are Go code, not data files: most records are
// accepted only alongside their authority records, and every write goes
// through the store's own gates, so a seeded row is exactly as trusted as one
// the daemon wrote itself.
//
// The fixture's execution admissions record the attended_dev capability class
// in AdmissionCapabilities. A store serving the fixture must be opened with an
// attended_dev floor that snapshot satisfies; without one the store refuses
// the admissions on write and fails their runs closed on read.
package seedfixture

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Name selects a fixture. The zero value "" is invalid by design.
type Name string

// Representative mirrors the app's mock inventory: two projects, runs in
// every lifecycle the task list shows, and one open item per attention type
// the generic store path can admit.
const Representative Name = "representative"

// AllNames is the single registration point for fixture names.
var AllNames = []Name{Representative}

func (n Name) valid() bool {
	switch n {
	case Representative:
		return true
	default:
		return false
	}
}

// Parse resolves a fixture name, listing the known names when it is unknown.
func Parse(raw string) (Name, error) {
	name := Name(raw)
	if !name.valid() {
		known := make([]string, len(AllNames))
		for i, n := range AllNames {
			known[i] = string(n)
		}
		return "", fmt.Errorf("unknown fixture %q (known: %s)", raw, strings.Join(known, ", "))
	}
	return name, nil
}

// ErrStoreNotEmpty reports a store that already holds runs or attention
// items. A fixture fills only an empty store; it never merges into state.
var ErrStoreNotEmpty = errors.New("store already holds runs or attention items")

// AdmissionCapabilities is the attended_dev capability snapshot every seeded
// execution admission records (see the package comment).
var AdmissionCapabilities = domain.NewCapabilitySnapshot(
	domain.CapDetachableWorkspace, domain.CapPostExitExport, domain.CapReadOnlyRemount,
)

// Inventory is the fixture's declared entity counts, which tests compare
// against what the store reads back.
type Inventory struct {
	Projects           int
	Tasks              int
	Runs               int
	AttentionItems     int
	WorkUnitPRBindings int
}

// Seed writes the named fixture into st, refusing a store that already holds
// runs or attention items. It is not atomic: the fixture spans the store's
// write and internal-write transactions, so an error after the first commit
// leaves a partial fixture, and the caller must discard the store to reseed.
func Seed(ctx context.Context, st *store.Store, name Name) (Inventory, error) {
	if err := requireEmpty(ctx, st); err != nil {
		return Inventory{}, err
	}
	switch name {
	case Representative:
		return seedRepresentative(ctx, st)
	}
	return Inventory{}, fmt.Errorf("seed fixture %q: %w", name, errUnknownName)
}

var errUnknownName = errors.New("unknown fixture name")

func requireEmpty(ctx context.Context, st *store.Store) error {
	return st.Read(ctx, func(tx *store.ReadTx) error {
		runs, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if len(runs) > 0 || len(items) > 0 {
			return ErrStoreNotEmpty
		}
		return nil
	})
}
