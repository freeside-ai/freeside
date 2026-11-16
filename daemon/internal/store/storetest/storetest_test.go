package storetest_test

import (
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestFollowUpFilingSeedsReachTheLedgerReads pins what the seeding helpers
// promise their callers: a dispatched filing makes the repository one the
// daemon may have created in while no issue is ledgered yet, a ledgered one
// names its issue, and neither reaches another repository.
func TestFollowUpFilingSeedsReachTheLedgerReads(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	storetest.LedgerFiledFollowUpIssue(t, st,
		storetest.FollowUpFiling{ProjectID: "project-ledgered", Repo: "owner/ledgered", RepositoryID: 101}, 41)
	storetest.DispatchFollowUpFiling(t, st,
		storetest.FollowUpFiling{ProjectID: "project-dispatched", Repo: "owner/dispatched", RepositoryID: 202})

	for _, tc := range []struct {
		name           string
		repositoryID   int64
		mayHaveCreated bool
		filed          bool
	}{
		{"ledgered", 101, true, true},
		{"dispatched", 202, true, false},
		{"untouched", 303, false, false},
	} {
		if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
			may, err := tx.FollowUpFilingMayHaveCreated(t.Context(), tc.repositoryID)
			if err != nil {
				return err
			}
			filed, err := tx.FiledFollowUpIssue(t.Context(), tc.repositoryID, 41)
			if err != nil {
				return err
			}
			if may != tc.mayHaveCreated || (filed != nil) != tc.filed {
				t.Errorf("%s repository: may have created = %v, issue 41 filed = %v; want %v, %v",
					tc.name, may, filed != nil, tc.mayHaveCreated, tc.filed)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s repository: %v", tc.name, err)
		}
	}
}
