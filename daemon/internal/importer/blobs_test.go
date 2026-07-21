package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// blobSpec describes one regular manifest entry for handoff fixtures.
type blobSpec struct {
	path    string
	content string
	mode    string // "" means 0644
	omitted bool
}

func sha256Digest(content string) export.Digest {
	sum := sha256.Sum256([]byte(content))
	return export.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// buildHandoff lays out a handoff directory exactly as the exporter
// would: an encoded manifest plus one content-addressed blob per
// non-omitted regular entry.
func buildHandoff(t *testing.T, specs []blobSpec) string {
	t.Helper()
	dir := t.TempDir()
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	entries := make([]export.Entry, 0, len(specs))
	for _, s := range specs {
		mode := s.mode
		if mode == "" {
			mode = "0644"
		}
		size := int64(len(s.content))
		digest := sha256Digest(s.content)
		entries = append(entries, export.Entry{
			Path: s.path, Kind: export.EntryRegular,
			Mode: &mode, Size: &size, Digest: &digest, BlobOmitted: s.omitted,
		})
		if !s.omitted {
			name := filepath.Join(blobDir, string(digest)[len("sha256:"):])
			if err := os.WriteFile(name, []byte(s.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	m := export.Manifest{Version: export.ManifestVersion, Entries: entries}
	body, err := m.Encode()
	if err != nil {
		t.Fatalf("encode fixture manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func loadFixtureManifest(t *testing.T, dir string) export.Manifest {
	t.Helper()
	m, err := loadManifest(dir, Policy{}.withDefaults())
	if err != nil {
		t.Fatalf("fixture manifest failed intake: %v", err)
	}
	return m
}

func verifyBlobsForTest(t *testing.T, dir string, m export.Manifest, pol Policy) (map[export.Digest]blobInfo, error) {
	t.Helper()
	// These tests exercise the repo channel; no evidence channel is present.
	repo, _, err := verifyBlobs(dir, t.TempDir(), m, export.EvidenceManifest{}, false, false, pol)
	return repo, err
}

func TestVerifyBlobsClean(t *testing.T) {
	dir := buildHandoff(t, []blobSpec{
		{path: "a.txt", content: "hello\n"},
		{path: "b/copy.txt", content: "hello\n"}, // dedup: one blob, two entries
		{path: "c.bin", content: "\x00\x01\x02"},
	})
	blobs, err := verifyBlobsForTest(t, dir, loadFixtureManifest(t, dir), Policy{}.withDefaults())
	if err != nil {
		t.Fatalf("verifyBlobs: %v", err)
	}
	if len(blobs) != 2 {
		t.Fatalf("got %d verified blobs, want 2 (dedup)", len(blobs))
	}
	// Known git object name for "hello\n" pins the blob-oid derivation
	// against real git (`git hash-object` of the same bytes).
	const helloOID = "ce013625030ba8dba906f756967f9e9ca394464a"
	info := blobs[sha256Digest("hello\n")]
	if info.gitOID != helloOID || info.size != 6 {
		t.Fatalf("hello blob info = %+v, want oid %s size 6", info, helloOID)
	}
}

// TestVerifyBlobsManyBatches exercises the batched blob-store
// enumeration across more than one ReadDir batch, so the multi-batch
// path is covered and a large legitimate store still audits clean.
func TestVerifyBlobsManyBatches(t *testing.T) {
	specs := make([]blobSpec, 0, dirBatch+50)
	for i := 0; i < dirBatch+50; i++ {
		specs = append(specs, blobSpec{path: fmt.Sprintf("f%05d.txt", i), content: fmt.Sprintf("content-%d\n", i)})
	}
	dir := buildHandoff(t, specs)
	blobs, err := verifyBlobsForTest(t, dir, loadFixtureManifest(t, dir), Policy{}.withDefaults())
	if err != nil {
		t.Fatalf("verifyBlobs across %d blobs: %v", len(specs), err)
	}
	if len(blobs) != len(specs) {
		t.Fatalf("verified %d blobs, want %d", len(blobs), len(specs))
	}
}

// TestVerifyBlobsRejectsOrphanAmongMany confirms a single orphan is
// still caught when the store spans multiple batches.
func TestVerifyBlobsRejectsOrphanAmongMany(t *testing.T) {
	specs := make([]blobSpec, 0, dirBatch+10)
	for i := 0; i < dirBatch+10; i++ {
		specs = append(specs, blobSpec{path: fmt.Sprintf("f%05d.txt", i), content: fmt.Sprintf("c-%d\n", i)})
	}
	dir := buildHandoff(t, specs)
	m := loadFixtureManifest(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", "deadbeef"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBlobsForTest(t, dir, m, Policy{}.withDefaults()); !errors.Is(err, ErrOrphanBlob) {
		t.Fatalf("verifyBlobs = %v, want %v", err, ErrOrphanBlob)
	}
}

// TestVerifyBlobsRejectsOverCap is the Codex round-6 regression: a
// stored blob whose declared size exceeds the per-file or total cap is
// rejected before it is opened or hashed, so the importer never streams
// an over-cap forged blob at the untrusted boundary.
func TestVerifyBlobsRejectsOverCap(t *testing.T) {
	t.Run("per-file cap", func(t *testing.T) {
		dir := buildHandoff(t, []blobSpec{{path: "a.txt", content: "hello\n"}})
		m := loadFixtureManifest(t, dir)
		pol := Policy{MaxBlobBytes: 3}.withDefaults() // below the 6-byte blob
		if _, err := verifyBlobsForTest(t, dir, m, pol); !errors.Is(err, ErrBlobTooLarge) {
			t.Fatalf("verifyBlobs = %v, want %v", err, ErrBlobTooLarge)
		}
	})
	t.Run("total cap", func(t *testing.T) {
		dir := buildHandoff(t, []blobSpec{
			{path: "a.txt", content: "aaaa\n"},
			{path: "b.txt", content: "bbbb\n"},
		})
		m := loadFixtureManifest(t, dir)
		pol := Policy{MaxTotalBytes: 7}.withDefaults() // two 5-byte blobs = 10
		if _, err := verifyBlobsForTest(t, dir, m, pol); !errors.Is(err, ErrBlobTooLarge) {
			t.Fatalf("verifyBlobs = %v, want %v", err, ErrBlobTooLarge)
		}
	})
	t.Run("dedup counts once", func(t *testing.T) {
		// Same content twice: 6 bytes total, not 12, so a 7-byte total
		// cap passes.
		dir := buildHandoff(t, []blobSpec{
			{path: "a.txt", content: "hello\n"},
			{path: "b.txt", content: "hello\n"},
		})
		m := loadFixtureManifest(t, dir)
		pol := Policy{MaxTotalBytes: 7}.withDefaults()
		if _, err := verifyBlobsForTest(t, dir, m, pol); err != nil {
			t.Fatalf("verifyBlobs = %v, want nil (dedup counts once)", err)
		}
	})
}

func TestVerifyBlobsEmptyStore(t *testing.T) {
	dir := buildHandoff(t, nil)
	blobs, err := verifyBlobsForTest(t, dir, loadFixtureManifest(t, dir), Policy{}.withDefaults())
	if err != nil || len(blobs) != 0 {
		t.Fatalf("verifyBlobs = %v, %v; want empty, nil", blobs, err)
	}
}

func TestVerifyBlobsRejects(t *testing.T) {
	digestOf := sha256Digest
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
		want   error
	}{
		{
			name: "tampered blob content",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				name := filepath.Join(dir, "blobs", "sha256", string(digestOf("hello\n"))[len("sha256:"):])
				if err := os.WriteFile(name, []byte("HELLO\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrDigestMismatch,
		},
		{
			name: "truncated blob",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				name := filepath.Join(dir, "blobs", "sha256", string(digestOf("hello\n"))[len("sha256:"):])
				if err := os.WriteFile(name, []byte("hell"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrSizeMismatch,
		},
		{
			// A blob file far larger than its claimed manifest size must
			// fail closed on length without streaming the whole file.
			name: "oversized blob",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				name := filepath.Join(dir, "blobs", "sha256", string(digestOf("hello\n"))[len("sha256:"):])
				big := append([]byte("hello\n"), make([]byte, 8<<20)...)
				if err := os.WriteFile(name, big, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrSizeMismatch,
		},
		{
			name: "missing blob",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				name := filepath.Join(dir, "blobs", "sha256", string(digestOf("hello\n"))[len("sha256:"):])
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrMissingBlob,
		},
		{
			name: "orphan blob file",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				name := filepath.Join(dir, "blobs", "sha256", "deadbeef")
				if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrOrphanBlob,
		},
		{
			name: "stray root entry",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "evil"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrOrphanBlob,
		},
		{
			name: "stray blob-store directory",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(dir, "blobs", "md5"), 0o750); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrOrphanBlob,
		},
		{
			name: "symlink in blob store",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				real := filepath.Join(dir, "blobs", "sha256", string(digestOf("hello\n"))[len("sha256:"):])
				if err := os.Remove(real); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/etc/passwd", real); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrOrphanBlob,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := buildHandoff(t, []blobSpec{{path: "a.txt", content: "hello\n"}})
			m := loadFixtureManifest(t, dir)
			tc.mutate(t, dir)
			_, err := verifyBlobsForTest(t, dir, m, Policy{}.withDefaults())
			if !errors.Is(err, tc.want) {
				t.Fatalf("verifyBlobs error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyBlobsRejectsOmittedBlobPresent(t *testing.T) {
	dir := buildHandoff(t, []blobSpec{{path: "big.bin", content: "oversized", omitted: true}})
	// The exporter never writes a blob for an omitted entry; plant one.
	name := filepath.Join(dir, "blobs", "sha256", string(sha256Digest("oversized"))[len("sha256:"):])
	if err := os.WriteFile(name, []byte("oversized"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := verifyBlobsForTest(t, dir, loadFixtureManifest(t, dir), Policy{}.withDefaults())
	if !errors.Is(err, ErrOrphanBlob) {
		t.Fatalf("verifyBlobs error = %v, want %v", err, ErrOrphanBlob)
	}
}

func TestOpenDirectoryRejectsSpecialDirectory(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, path string)
	}{
		{
			name: "symlink",
			plant: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fifo",
			plant: func(t *testing.T, path string) {
				t.Helper()
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Skipf("mkfifo unsupported: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "replaced")
			tc.plant(t, path)
			// openDirectory is the pathname boundary the importer opens the
			// handoff root through; O_NONBLOCK must make even a FIFO open
			// return at once instead of blocking for a writer, so the
			// goroutine + timeout proves it never hangs.
			done := make(chan error, 1)
			go func() {
				d, err := openDirectory(path, ErrHandoffUnreadable)
				if d != nil {
					_ = d.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				// A symlink is refused by O_NOFOLLOW (ELOOP); a FIFO fails
				// O_DIRECTORY (ENOTDIR). Either way the special inode never
				// becomes a scannable handoff directory.
				if !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) {
					t.Fatalf("openDirectory = %v, want ELOOP or ENOTDIR", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("openDirectory blocked on a replaced handoff directory")
			}
		})
	}
}

func TestOpenDirectoryAtPinsAndRejectsChildren(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "blobs", "sha256"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := openDirectory(rootPath, ErrHandoffUnreadable)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	blobs, err := openDirectoryAt(root, "blobs", ErrHandoffUnreadable)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	// Replace the pathname for the already-open parent. The pinned blobs
	// descriptor must still open its original sha256 child, never traverse
	// through the replacement symlink.
	if err := os.Rename(filepath.Join(rootPath, "blobs"), filepath.Join(rootPath, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(rootPath, "blobs")); err != nil {
		t.Fatal(err)
	}
	sha256, err := openDirectoryAt(blobs, "sha256", ErrHandoffUnreadable)
	if err != nil {
		t.Fatalf("open child of pinned parent: %v", err)
	}
	_ = sha256.Close()
	if _, err := openDirectoryAt(root, "blobs", ErrHandoffUnreadable); !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("open swapped symlink child = %v, want ELOOP or ENOTDIR", err)
	}
}

func TestVerifyBlobsRejectsConflictingSizes(t *testing.T) {
	// Two entries claim the same digest at different sizes: one lies.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o750); err != nil {
		t.Fatal(err)
	}
	digest := sha256Digest("hello\n")
	mode := "0644"
	size6, size7 := int64(6), int64(7)
	m := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{
		{Path: "a.txt", Kind: export.EntryRegular, Mode: &mode, Size: &size6, Digest: &digest},
		{Path: "b.txt", Kind: export.EntryRegular, Mode: &mode, Size: &size7, Digest: &digest},
	}}
	_, err := verifyBlobsForTest(t, dir, m, Policy{}.withDefaults())
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("verifyBlobs error = %v, want %v", err, ErrSizeMismatch)
	}
}
