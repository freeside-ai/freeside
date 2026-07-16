package export

import (
	"bytes"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
)

// TestWalkWorkspaceClassification drives the walker over an in-memory
// hostile workspace: fstest.MapFS can fake every mode class (devices,
// sockets, setuid, non-UTF-8 names) without privileges or filesystem
// support, so the exotic classifications stay covered identically on
// darwin and linux. Regular entries carry no digest yet; the blob step
// fills it.
func TestWalkWorkspaceClassification(t *testing.T) {
	fsys := fstest.MapFS{
		".git/config":          {Data: []byte("[core]\n")},
		"bad\xffdir":           {Mode: fs.ModeDir},
		"bad\xffdir/child":     {Data: []byte("hidden")},
		"bad\xffname":          {Data: []byte("x")},
		"dev/fake-disk":        {Mode: fs.ModeDevice},
		"fifo/queue":           {Mode: fs.ModeNamedPipe},
		"link/rel":             {Mode: fs.ModeSymlink, Data: []byte("../target")},
		"nul\x00file":          {Data: []byte("z")},
		"plain.txt":            {Data: []byte("hello\n")},
		"sock/api.sock":        {Mode: fs.ModeSocket},
		"sticky/scratch":       {Data: []byte("s"), Mode: 0o644 | fs.ModeSticky},
		"sub/vendored/.git":    {Mode: fs.ModeDir},
		"sub/vendored/code.go": {Data: []byte("package x\n")},
		"suid/helper":          {Data: []byte("bin"), Mode: 0o755 | fs.ModeSetuid},
		"tools/run.sh":         {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
	}

	got, err := walkWorkspace(fsys, 0)
	if err != nil {
		t.Fatalf("walkWorkspace: %v", err)
	}

	want := []Entry{
		{Path: ".git", Kind: EntryGitDir},
		{PathHex: "626164ff646972", Kind: EntryInvalidPath},   // "bad\xffdir"; child not listed
		{PathHex: "626164ff6e616d65", Kind: EntryInvalidPath}, // "bad\xffname"
		{Path: "dev/fake-disk", Kind: EntrySpecial},
		{Path: "fifo/queue", Kind: EntrySpecial},
		{Path: "link/rel", Kind: EntrySymlink, Target: ptrTo("../target")},
		// "nul\x00file": valid UTF-8, but NUL can appear on no real
		// filesystem, so it takes the lossless invalid_path form.
		{PathHex: "6e756c0066696c65", Kind: EntryInvalidPath},
		{Path: "plain.txt", Kind: EntryRegular, Mode: ptrTo("0644"), Size: ptrTo(int64(6))},
		{Path: "sock/api.sock", Kind: EntrySpecial},
		{Path: "sticky/scratch", Kind: EntryUnusualMode, Mode: ptrTo("01644")},
		{Path: "sub/vendored", Kind: EntrySubmodule},
		{Path: "suid/helper", Kind: EntryUnusualMode, Mode: ptrTo("04755")},
		{Path: "tools/run.sh", Kind: EntryRegular, Mode: ptrTo("0755"), Size: ptrTo(int64(10))},
	}
	assertEntriesEqual(t, want, got)
}

// TestWalkWorkspaceRootGitFile covers the linked-worktree form: a
// workspace whose top-level .git is a file, not a directory.
func TestWalkWorkspaceRootGitFile(t *testing.T) {
	fsys := fstest.MapFS{
		".git":    {Data: []byte("gitdir: /elsewhere\n")},
		"main.go": {Data: []byte("package main\n")},
	}

	got, err := walkWorkspace(fsys, 0)
	if err != nil {
		t.Fatalf("walkWorkspace: %v", err)
	}
	want := []Entry{
		{Path: ".git", Kind: EntryGitDir},
		{Path: "main.go", Kind: EntryRegular, Mode: ptrTo("0644"), Size: ptrTo(int64(13))},
	}
	assertEntriesEqual(t, want, got)
}

// unknownTypeFS wraps a MapFS but reports every directory entry's type as
// unknown (zero), as FUSE/NFS-style backends can; classification must fall
// back to the lstat mode, never assume regular. Open is the interception
// point because the walker enumerates via ReadDirFile batches.
type unknownTypeFS struct{ fstest.MapFS }

func (u unknownTypeFS) Open(name string) (fs.File, error) {
	f, err := u.MapFS.Open(name)
	if err != nil {
		return nil, err
	}
	if rd, ok := f.(fs.ReadDirFile); ok {
		return unknownTypeDir{f, rd}, nil
	}
	return f, nil
}

type unknownTypeDir struct {
	fs.File
	rd fs.ReadDirFile
}

func (d unknownTypeDir) ReadDir(n int) ([]fs.DirEntry, error) {
	des, err := d.rd.ReadDir(n)
	wrapped := make([]fs.DirEntry, len(des))
	for i, de := range des {
		wrapped[i] = unknownTypeEntry{de}
	}
	return wrapped, err
}

type unknownTypeEntry struct{ fs.DirEntry }

func (unknownTypeEntry) Type() fs.FileMode { return 0 }

// TestWalkWorkspaceUnknownDirEntryType: with the backend claiming every
// type unknown, only the lstat-proven regular file may classify as
// regular; the FIFO and symlink must not be misread as openable content.
func TestWalkWorkspaceUnknownDirEntryType(t *testing.T) {
	fsys := unknownTypeFS{fstest.MapFS{
		"fifo":      {Mode: fs.ModeNamedPipe},
		"link":      {Mode: fs.ModeSymlink, Data: []byte("target")},
		"plain.txt": {Data: []byte("hello\n")},
	}}

	got, err := walkWorkspace(fsys, 0)
	if err != nil {
		t.Fatalf("walkWorkspace: %v", err)
	}
	want := []Entry{
		{Path: "fifo", Kind: EntrySpecial},
		{Path: "link", Kind: EntrySymlink, Target: ptrTo("target")},
		{Path: "plain.txt", Kind: EntryRegular, Mode: ptrTo("0644"), Size: ptrTo(int64(6))},
	}
	assertEntriesEqual(t, want, got)
}

// TestWalkSymlinkTargetNonUTF8 pins the acknowledged best-effort handling
// of a non-UTF-8 symlink target: the walker records the raw bytes
// losslessly, but json encoding folds them to U+FFFD. Two distinct such
// targets can therefore collide in the encoded manifest — accepted because
// every symlink is publish-blocking downstream regardless of target, so
// the target is informational, never an identity the importer binds to.
func TestWalkSymlinkTargetNonUTF8(t *testing.T) {
	fsys := fstest.MapFS{
		"link": {Mode: fs.ModeSymlink, Data: []byte("bad\xfftarget")},
	}
	got, err := walkWorkspace(fsys, 0)
	if err != nil {
		t.Fatalf("walkWorkspace: %v", err)
	}
	if len(got) != 1 || got[0].Kind != EntrySymlink || got[0].Target == nil {
		t.Fatalf("got %+v, want one symlink entry", got)
	}
	if *got[0].Target != "bad\xfftarget" {
		t.Errorf("walker target = %q, want the raw bytes preserved", *got[0].Target)
	}
	m := Manifest{Version: ManifestVersion, Entries: got}
	body, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// encoding/json emits the U+FFFD replacement as a six-byte \u escape,
	// so two distinct invalid targets both encode to this.
	if !bytes.Contains(body, []byte("\\ufffd")) {
		t.Error("expected the non-UTF-8 target to fold to U+FFFD in the encoded manifest")
	}
}

func assertEntriesEqual(t *testing.T, want, got []Entry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("entry count = %d, want %d\ngot: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(describe(want[i]), describe(got[i])) {
			t.Errorf("entry %d = %+v, want %+v", i, describe(got[i]), describe(want[i]))
		}
	}
}

// describe flattens an Entry's pointer fields so test failures print
// values, not addresses.
func describe(e Entry) map[string]any {
	m := map[string]any{"path": e.Path, "path_hex": e.PathHex, "kind": e.Kind, "blob_omitted": e.BlobOmitted}
	if e.Mode != nil {
		m["mode"] = *e.Mode
	}
	if e.Size != nil {
		m["size"] = *e.Size
	}
	if e.Digest != nil {
		m["digest"] = *e.Digest
	}
	if e.Target != nil {
		m["target"] = *e.Target
	}
	return m
}
