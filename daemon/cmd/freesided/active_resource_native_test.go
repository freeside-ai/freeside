package main

import (
	"context"
	"database/sql"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

const testReviewerLogin = "chatgpt-codex-connector"

func testReviewers() map[string]bool { return map[string]bool{testReviewerLogin: true} }

func staticReview(obs publish.PullReviewObservation) daemoncompose.NativeReviewObserver {
	return func(context.Context, string, int) (publish.PullReviewObservation, error) {
		return obs, nil
	}
}

// execStoreSQL runs one statement through a second connection to the store's
// file. Tests use it to damage the store, and to repair it, behind the
// reconciler's back.
func execStoreSQL(t *testing.T, dbPath, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open store db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}
