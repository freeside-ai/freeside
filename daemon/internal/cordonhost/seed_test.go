package cordonhost

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSeedRoundTripsACheckout(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "src")
	for path, body := range map[string]string{
		"README.md":         "hello\n",
		".git/HEAD":         "ref: refs/heads/main\n",
		".git/objects/ab/c": "object",
		"script.sh":         "#!/bin/sh\n",
	} {
		full := filepath.Join(src, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil { //nolint:gosec // G301: a checkout's ordinary modes, which the seed must narrow
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil { //nolint:gosec // G306: a checkout's ordinary modes, which the seed must narrow
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(src, "script.sh"), 0o755); err != nil { //nolint:gosec // G302: an executable in a checkout, which the seed must narrow
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "empty"), 0o755); err != nil { //nolint:gosec // G301: a checkout's ordinary modes, which the seed must narrow
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := writeTree(&stream, src); err != nil {
		t.Fatal(err)
	}
	stream.WriteString("after")
	dst := filepath.Join(t.TempDir(), "dst")
	if err := readTree(&stream, dst, seedRules); err != nil {
		t.Fatalf("readTree: %v", err)
	}
	if rest := stream.String(); rest != "after" {
		t.Fatalf("the seed read past its end, leaving %q", rest)
	}
	for path, want := range map[string]string{"README.md": "hello\n", ".git/HEAD": "ref: refs/heads/main\n", ".git/objects/ab/c": "object"} {
		if got, err := os.ReadFile(filepath.Join(dst, path)); err != nil || string(got) != want { //nolint:gosec // G304: the test's own extracted tree
			t.Errorf("%s = %q, %v", path, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(dst, "script.sh")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("script.sh mode %v, %v", info.Mode(), err)
	}
	if info, err := os.Stat(filepath.Join(dst, "README.md")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("README.md mode %v, %v", info.Mode(), err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "README.md" {
		t.Errorf("link = %q, %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !info.IsDir() {
		t.Errorf("empty = %v, %v", info, err)
	}
}

// seedOf frames a hand-built tar as a seed stream.
func seedOf(t *testing.T, build func(*tar.Writer)) *bytes.Buffer {
	t.Helper()
	var stream bytes.Buffer
	cw := chunkWriter{w: &stream}
	tw := tar.NewWriter(cw)
	build(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cw.end(); err != nil {
		t.Fatal(err)
	}
	return &stream
}

// TestReadSeedRefusesWhatWriteSeedNeverWrites shows a seed that names a
// path outside its directory, writes through a link, or carries another
// entry type or mode lands nothing outside the directory and is refused.
func TestReadSeedRefusesWhatWriteSeedNeverWrites(t *testing.T) {
	t.Parallel()
	file := func(tw *tar.Writer, name string, mode int64) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: 1})
		_, _ = tw.Write([]byte("x"))
	}
	cases := map[string]func(*tar.Writer){
		"parent":   func(tw *tar.Writer) { file(tw, "../escape", 0o600) },
		"absolute": func(tw *tar.Writer) { file(tw, "/tmp/escape", 0o600) },
		"mode":     func(tw *tar.Writer) { file(tw, "setuid", 0o4700) },
		"through a link": func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Name: "out", Typeflag: tar.TypeSymlink, Linkname: "..", Mode: 0o700})
			file(tw, "out/escape", 0o600)
		},
		"hard link": func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "/etc/hosts"})
		},
		"device": func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Name: "dev", Typeflag: tar.TypeChar, Mode: 0o600})
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			dst := filepath.Join(parent, "dst")
			if err := readTree(seedOf(t, build), dst, seedRules); err == nil {
				t.Fatal("the seed was accepted")
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "dst" {
				t.Fatalf("the seed reached %v beside its directory", entries)
			}
		})
	}
	var oversized bytes.Buffer
	oversized.Write([]byte{0, 1, 0, 1})
	if err := readTree(&oversized, filepath.Join(t.TempDir(), "dst"), seedRules); !errors.Is(err, ErrProtocol) {
		t.Fatalf("an oversized chunk = %v, want ErrProtocol", err)
	}
}
