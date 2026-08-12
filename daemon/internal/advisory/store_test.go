package advisory_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

func TestStoreRetainsNewestEntriesAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "advisory.json")
	store, err := advisory.Open(path, 2, 64)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"a", "b", "c"} {
		entry := advisory.Entry{
			ID: id, RootLineage: "run-1", Site: "diagnostic", Producer: "fake/test",
			Kind: "claim", InputDigest: contentaddr.Sum([]byte(id)), Body: id,
			CreatedAt: now, RetainUntil: now.Add(time.Hour),
		}
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := advisory.Open(path, 2, 64)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "c" {
		t.Fatalf("retained entries = %#v, want b,c", got)
	}
}

func TestPrunePhysicallyRemovesExpiredBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "advisory.json")
	now := time.Unix(100, 0).UTC()
	store, err := advisory.Open(path, 2, 64, advisory.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	entry := advisory.Entry{
		ID: "expired", RootLineage: "run-1", Site: "diagnostic", Producer: "fake/test",
		Kind: "claim", InputDigest: contentaddr.Sum([]byte("expired")), Body: "remove me",
		CreatedAt: now, RetainUntil: now.Add(time.Minute),
	}
	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if err := store.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Entries []advisory.Entry `json:"entries"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Entries) != 0 || strings.Contains(string(body), entry.Body) {
		t.Fatalf("expired advisory body remains on disk: %s", body)
	}
}

func TestStoreRejectsOversizeAndIdentityRewrite(t *testing.T) {
	store, err := advisory.Open(filepath.Join(t.TempDir(), "advisory.json"), 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	base := advisory.Entry{
		ID: "a", RootLineage: "run-1", Site: "diagnostic", Producer: "fake/test",
		Kind: "claim", InputDigest: contentaddr.Sum([]byte("a")), Body: "ok",
		CreatedAt: now, RetainUntil: now.Add(time.Hour),
	}
	if err := store.Append(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Body = "new"
	if err := store.Append(context.Background(), changed); err == nil {
		t.Fatal("identity rewrite succeeded")
	}
	base.ID = "b"
	base.Body = "long"
	if err := store.Append(context.Background(), base); err == nil {
		t.Fatal("oversize entry succeeded")
	}
}
