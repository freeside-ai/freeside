package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// deputyWaitRoot is a deputy state root's database and manifest paths.
func deputyWaitRoot(t *testing.T) (db, manifest string) {
	t.Helper()
	root := t.TempDir()
	return filepath.Join(root, "freeside.db"), newDeputyPaths(root).Manifest
}

func writeWaitFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForStartingDeputyReturnsOnceItServes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		db, manifest := deputyWaitRoot(t)
		writeWaitFile(t, newDeputyPaths(filepath.Dir(db)).Acquisition)
		go func() {
			time.Sleep(2 * time.Second)
			writeWaitFile(t, manifest)
			time.Sleep(3 * time.Second)
			writeWaitFile(t, db+".control.json")
		}()
		var stderr bytes.Buffer
		start := time.Now()
		if err := waitForStartingDeputy(t.Context(), db, time.Minute, &stderr); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited < 5*time.Second || waited > 5*time.Second+deputyStartPoll {
			t.Fatalf("waited %s, want until the control address appeared at 5s", waited)
		}
		if !strings.Contains(stderr.String(), "waiting up to 1m0s for the deputy on "+db) {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})
}

func TestWaitForStartingDeputyFailsAtTheBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		db, manifest := deputyWaitRoot(t)
		writeWaitFile(t, newDeputyPaths(filepath.Dir(db)).Acquisition)
		writeWaitFile(t, manifest)
		start := time.Now()
		err := waitForStartingDeputy(t.Context(), db, time.Minute, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "the deputy on "+db+" is still starting after 1m0s") ||
			!strings.Contains(err.Error(), `"ready for submissions"`) {
			t.Fatalf("err = %v", err)
		}
		if waited := time.Since(start); waited != time.Minute {
			t.Fatalf("waited %s, want the 1m bound", waited)
		}
	})
}

func TestWaitForStartingDeputyDoesNotWaitWithoutAStartingDeputy(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		db, _ := deputyWaitRoot(t)
		var stderr bytes.Buffer
		start := time.Now()
		if err := waitForStartingDeputy(t.Context(), db, time.Minute, &stderr); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != 0 || stderr.Len() != 0 {
			t.Fatalf("waited %s and wrote %q with no rig acquisition", waited, stderr.String())
		}
	})
}

// A start whose agent tree didn't match serves without a manifest, so the
// wait ends at the control address and submit reports the missing manifest.
func TestWaitForStartingDeputyEndsWhenAFailedStartServes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		db, _ := deputyWaitRoot(t)
		writeWaitFile(t, newDeputyPaths(filepath.Dir(db)).Acquisition)
		writeWaitFile(t, db+".control.json")
		start := time.Now()
		if err := waitForStartingDeputy(t.Context(), db, time.Minute, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("waited %s for a manifest a failed start never writes", waited)
		}
	})
}
