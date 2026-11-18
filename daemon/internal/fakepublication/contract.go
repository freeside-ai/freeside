// Package fakepublication keeps the item IDs production's ready and blocked
// items inherited from the retired attended fake publication lane (#2790).
// Every stored ready and blocked item is keyed by these IDs, so their
// strings must never change.
package fakepublication

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// taskKind is the retired lane's outbox kind. No code reads or writes that
// kind any more, but every item ID hashes it, so changing it would orphan
// every stored ready and blocked item.
const taskKind = "engine.fake_publication"

func ReadyItemID(runID domain.RunID) domain.ItemID { return terminalItemID("ready", runID) }

func BlockedItemID(runID domain.RunID) domain.ItemID { return terminalItemID("blocked", runID) }

func terminalItemID(kind string, runID domain.RunID) domain.ItemID {
	sum := sha256.Sum256([]byte(taskKind + "\x00" + kind + "\x00" + string(runID)))
	return domain.ItemID("fake-publication-" + kind + "-" + hex.EncodeToString(sum[:]))
}
