package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// The daemon's advisory store admits an audit sample as large as any judgment
// site's whole output, and nothing larger (#2482). It is opened through the
// helper main uses, over the sites main registers.
func TestAdvisoryStoreAdmitsEverySiteWholeOutput(t *testing.T) {
	sites := judgmentTestSites()
	store, err := openAdvisoryStore(t.TempDir(), sites)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC()
	sample := func(id, site string, size int) advisory.Entry {
		return advisory.Entry{
			ID: id, RootLineage: "run-1", Site: site, Producer: "fake", Kind: "audit_sample",
			InputDigest: contentaddr.Sum([]byte(id)), Body: strings.Repeat("x", size),
			CreatedAt: created, RetainUntil: created.Add(time.Hour),
		}
	}
	for _, site := range sites {
		if err := store.Append(context.Background(), sample(site.ID, site.ID, site.MaxOutputBytes)); err != nil {
			t.Fatalf("sample of site %s at its %d-byte cap: %v", site.ID, site.MaxOutputBytes, err)
		}
	}
	largest := inference.MaxSiteOutputBytes(sites)
	err = store.Append(context.Background(), sample("over", sites[0].ID, largest+1))
	if !errors.Is(err, advisory.ErrInvalidEntry) {
		t.Fatalf("sample one byte over the largest cap %d: %v", largest, err)
	}
}
