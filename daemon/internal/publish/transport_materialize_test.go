package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

func TestCappedMaterializationBufferStopsStreamingCommand(t *testing.T) {
	t.Parallel()
	buffer := cappedMaterializationBuffer{remaining: 3}
	n, err := buffer.Write([]byte("abcdef"))
	if n != 3 || !errors.Is(err, errMaterializationTreeListingLimit) {
		t.Fatalf("overflow write = (%d, %v), want (3, listing limit)", n, err)
	}
	if got := string(buffer.Bytes()); got != "abc" {
		t.Fatalf("retained bytes = %q, want abc", got)
	}

	producer := filepath.Join(t.TempDir(), "producer.sh")
	if err := os.WriteFile(
		producer,
		[]byte("#!/bin/sh\nwhile :; do printf '0123456789abcdef0123456789abcdef\\n'; done\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(producer, 0o700); err != nil { //nolint:gosec // executable test-owned producer
		t.Fatal(err)
	}
	runner, err := newNetRunner(producer, t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	streamed := cappedMaterializationBuffer{remaining: 32}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := runner.runTo(ctx, &streamed, "ls-tree"); !errors.Is(err, errMaterializationTreeListingLimit) {
		t.Fatalf("unbounded command error = %v, want listing limit", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("writer did not stop command before context deadline: %v", ctx.Err())
	}
	if !streamed.exceeded || len(streamed.Bytes()) != 32 {
		t.Fatalf("stream cap = (exceeded %v, bytes %d), want (true, 32)", streamed.exceeded, len(streamed.Bytes()))
	}
}

func TestRetainedRepositoryReaderStopsOnCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reader := retainedRepositoryReader{ctx: ctx, r: strings.NewReader("repository bytes")}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled repository read error = %v, want context cancellation", err)
	}
}

func TestRetainWorktreePopulatesImportedCandidate(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(
		t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t),
	)
	if err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	want := map[string][]byte{
		"a.txt":          []byte("candidate\n"),
		"nested/tool.sh": []byte("#!/bin/sh\necho candidate\n"),
	}
	for path, body := range want {
		full := filepath.Join(workspace, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(workspace, "nested", "tool.sh"), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	handoff := filepath.Join(t.TempDir(), "handoff")
	if _, err := export.Export(os.DirFS(workspace), handoff, export.Options{}); err != nil {
		t.Fatal(err)
	}
	imported, err := importer.Import(t.Context(), handoff, co.Dir(), importer.Options{
		BaseSHA: co.BaseSHA(), CommitDate: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if imported.CommitSHA == "" {
		t.Fatal("import produced no candidate commit")
	}
	batchLog := filepath.Join(t.TempDir(), "cat-file-batches")
	gitWrapper := filepath.Join(t.TempDir(), "git-wrapper.sh")
	if err := os.WriteFile(gitWrapper, []byte(fmt.Sprintf(`#!/bin/sh
for arg
do
	if [ "$arg" = "--batch" ]; then
		printf 'batch\n' >> %q
	fi
done
exec git "$@"
`, batchLog)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gitWrapper, 0o700); err != nil { //nolint:gosec // executable test-owned git wrapper
		t.Fatal(err)
	}
	remote.transport.gitPath = gitWrapper

	retained := filepath.Join(t.TempDir(), "review-workspace")
	if err := os.WriteFile(filepath.Join(co.Dir(), "stale.txt"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceBefore := snapshotTree(t, co.Dir())
	if err := remote.transport.RetainWorktree(t.Context(), co.Dir(), retained, imported.CommitSHA); err != nil {
		t.Fatal(err)
	}
	batchCalls, err := os.ReadFile(batchLog) //nolint:gosec // test-owned invocation log
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(batchCalls), "batch\n"); got != 1 {
		t.Fatalf("cat-file batch processes = %d, want 1 for %d blobs", got, len(want))
	}
	assertTreeUnchanged(t, co.Dir(), sourceBefore)
	if _, err := os.Lstat(filepath.Join(retained, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale path survived materialization: %v", err)
	}
	for path, wantBody := range want {
		body, err := os.ReadFile(filepath.Join(retained, filepath.FromSlash(path))) //nolint:gosec // test-owned path map and fixture tree
		if err != nil {
			t.Fatalf("read tracked path %q: %v", path, err)
		}
		if !reflect.DeepEqual(body, wantBody) {
			t.Errorf("tracked path %q = %q, want %q", path, body, wantBody)
		}
	}
	if paths := strings.Fields(gitOut(t, retained, "ls-tree", "-r", "--name-only", "HEAD")); !reflect.DeepEqual(paths, []string{"a.txt", "nested/tool.sh"}) {
		t.Errorf("tracked paths = %q", paths)
	}
	if _, err := os.Stat(filepath.Join(retained, ".git", "index")); err != nil {
		t.Fatalf("checkout-owned index is missing: %v", err)
	}
	if status := gitOut(t, retained, "status", "--porcelain"); status != "" {
		t.Errorf("materialized worktree is dirty:\n%s", status)
	}
	if head := gitOut(t, retained, "rev-parse", "HEAD"); head != imported.CommitSHA {
		t.Errorf("materialized HEAD = %s, want %s", head, imported.CommitSHA)
	}

	afterFirst := snapshotWorktree(t, retained)
	if err := remote.transport.RetainWorktree(t.Context(), co.Dir(), retained, imported.CommitSHA); err == nil {
		t.Fatal("second retention replaced an existing destination")
	}
	if afterSecond := snapshotWorktree(t, retained); !reflect.DeepEqual(afterSecond, afterFirst) {
		t.Error("refused second retention changed the worktree")
	}
	if status := gitOut(t, retained, "status", "--porcelain"); status != "" {
		t.Errorf("second materialization left the worktree dirty:\n%s", status)
	}
}

func TestRetainWorktreeRefusesWithoutSideEffects(t *testing.T) {
	t.Parallel()
	t.Run("oversized repository", func(t *testing.T) {
		remote := newLocalRemote(t)
		co, err := remote.transport.FetchBase(
			t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t),
		)
		if err != nil {
			t.Fatal(err)
		}
		oversized := filepath.Join(co.Dir(), ".git", "oversized-object-database")
		if err := os.WriteFile(oversized, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(oversized, maxRetainedRepositoryBytes+1); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "retained")
		err = remote.transport.RetainWorktree(t.Context(), co.Dir(), dest, remote.baseSHA)
		if !errors.Is(err, ErrMaterializationRefused) {
			t.Fatalf("oversized repository error = %v, want materialization refusal", err)
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatalf("oversized repository retention left a destination: %v", err)
		}
		info, err := os.Stat(oversized)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != maxRetainedRepositoryBytes+1 {
			t.Fatalf("source repository file size = %d, want unchanged", info.Size())
		}
	})

	t.Run("malformed commit", func(t *testing.T) {
		remote := newLocalRemote(t)
		co, err := remote.transport.FetchBase(
			t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t),
		)
		if err != nil {
			t.Fatal(err)
		}
		before := snapshotTree(t, co.Dir())
		dest := filepath.Join(t.TempDir(), "retained")
		if err := remote.transport.RetainWorktree(t.Context(), co.Dir(), dest, "not-a-sha"); err == nil {
			t.Fatal("malformed commit SHA was accepted")
		}
		assertTreeUnchanged(t, co.Dir(), before)
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatalf("malformed retention left a destination: %v", err)
		}
	})

	t.Run("absent commit", func(t *testing.T) {
		remote := newLocalRemote(t)
		co, err := remote.transport.FetchBase(
			t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t),
		)
		if err != nil {
			t.Fatal(err)
		}
		before := snapshotTree(t, co.Dir())
		missing := strings.Repeat("deadbeef", 5)
		dest := filepath.Join(t.TempDir(), "retained")
		if err := remote.transport.RetainWorktree(t.Context(), co.Dir(), dest, missing); err == nil {
			t.Fatal("absent commit was accepted")
		}
		assertTreeUnchanged(t, co.Dir(), before)
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatalf("absent retention left a destination: %v", err)
		}
	})

	t.Run("foreign git directory", func(t *testing.T) {
		remote := newLocalRemote(t)
		outer := t.TempDir()
		gitOut(t, "", "init", "--initial-branch=main", outer)
		if err := os.WriteFile(filepath.Join(outer, "owned.txt"), []byte("owned\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitOut(t, outer, "add", "owned.txt")
		gitOut(t, outer, "commit", "-m", "owned")
		inner := filepath.Join(outer, "nested", "checkout")
		if err := os.MkdirAll(inner, 0o700); err != nil {
			t.Fatal(err)
		}
		head := gitOut(t, outer, "rev-parse", "HEAD")
		before := snapshotTree(t, outer)
		dest := filepath.Join(t.TempDir(), "retained")
		if err := remote.transport.RetainWorktree(t.Context(), inner, dest, head); err == nil {
			t.Fatal("directory resolving to a foreign git dir was accepted")
		}
		assertTreeUnchanged(t, outer, before)
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatalf("foreign retention left a destination: %v", err)
		}
	})

	t.Run("destination parent aliases source", func(t *testing.T) {
		remote := newLocalRemote(t)
		co, err := remote.transport.FetchBase(
			t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t),
		)
		if err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(t.TempDir(), "source-alias")
		if err := os.Symlink(co.Dir(), alias); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(alias, "retained")
		before := snapshotTree(t, co.Dir())
		if err := remote.transport.RetainWorktree(
			t.Context(), co.Dir(), dest, remote.baseSHA,
		); err == nil {
			t.Fatal("destination physically inside source was accepted")
		}
		assertTreeUnchanged(t, co.Dir(), before)
		if _, err := os.Lstat(filepath.Join(co.Dir(), "retained")); !os.IsNotExist(err) {
			t.Fatalf("aliased retention changed the source: %v", err)
		}
	})

	for _, fixture := range []struct {
		name string
		add  func(*testing.T, *localRemote)
	}{
		{
			name: "symlink",
			add: func(t *testing.T, remote *localRemote) {
				t.Helper()
				if err := os.Symlink("a.txt", filepath.Join(remote.work, "link")); err != nil {
					t.Fatal(err)
				}
				gitOut(t, remote.work, "add", "link")
			},
		},
		{
			name: "gitlink",
			add: func(t *testing.T, remote *localRemote) {
				t.Helper()
				gitOut(t, remote.work, "update-index", "--add", "--cacheinfo", "160000,"+remote.baseSHA+",module")
			},
		},
		{
			name: "line-break path",
			add: func(t *testing.T, remote *localRemote) {
				t.Helper()
				name := "line\nbreak"
				if err := os.WriteFile(filepath.Join(remote.work, name), []byte("unsupported\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitOut(t, remote.work, "add", name)
			},
		},
		{
			name: "working-tree-encoding attributes",
			add: func(t *testing.T, remote *localRemote) {
				t.Helper()
				if err := os.WriteFile(
					filepath.Join(remote.work, ".gitattributes"),
					[]byte("*.txt working-tree-encoding=UTF-16\n"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
				gitOut(t, remote.work, "add", ".gitattributes")
			},
		},
	} {
		t.Run(fixture.name+" tree", func(t *testing.T) {
			remote := newLocalRemote(t)
			fixture.add(t, remote)
			gitOut(t, remote.work, "commit", "-m", fixture.name)
			head := gitOut(t, remote.work, "rev-parse", "HEAD")
			gitOut(t, remote.work, "push", remote.bare, "main:main")
			co, err := remote.transport.FetchBase(
				t.Context(), remote.repo, "main", head, checkoutDir(t),
			)
			if err != nil {
				t.Fatal(err)
			}
			before := snapshotTree(t, co.Dir())
			dest := filepath.Join(t.TempDir(), "retained")
			err = remote.transport.RetainWorktree(t.Context(), co.Dir(), dest, head)
			if !errors.Is(err, ErrMaterializationRefused) || !errors.Is(err, ErrGitTransport) {
				t.Fatalf("%s-bearing tree error = %v, want materialization and transport refusal", fixture.name, err)
			}
			assertTreeUnchanged(t, co.Dir(), before)
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("%s retention left a destination: %v", fixture.name, err)
			}
		})
	}
}

func TestFetchBaseWorktreeRefusesWorkingTreeEncoding(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	if err := os.WriteFile(
		filepath.Join(remote.work, ".gitattributes"),
		[]byte("*.txt working-tree-encoding=UTF-16\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	gitOut(t, remote.work, "add", ".gitattributes")
	gitOut(t, remote.work, "commit", "-m", "working tree encoding")
	head := gitOut(t, remote.work, "rev-parse", "HEAD")
	gitOut(t, remote.work, "push", remote.bare, "main:main")
	dest := checkoutDir(t)
	_, err := remote.transport.FetchBaseWorktree(
		t.Context(), remote.repo, "main", head, dest,
	)
	if !errors.Is(err, ErrMaterializationRefused) || !errors.Is(err, ErrGitTransport) {
		t.Fatalf("implementer seed error = %v, want materialization and transport refusal", err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("refused implementer seed left a checkout: %v", err)
	}
}

func TestWorkingTreeEncodingDetector(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		chunks []string
		want   bool
	}{
		{name: "assigned", chunks: []string{"*.txt working-tree-encoding=UTF-16\n"}, want: true},
		{name: "macro split across writes", chunks: []string{"[attr]encoded working-tree-", "encoding=UTF-16\n"}, want: true},
		{name: "unset", chunks: []string{"*.txt -working-tree-encoding\n"}},
		{name: "unspecified", chunks: []string{"*.txt !working-tree-encoding\n"}},
		{name: "comment", chunks: []string{"# *.txt working-tree-encoding=UTF-16\n"}},
		{name: "pattern only", chunks: []string{"working-tree-encoding ordinary-attribute\n"}},
		{
			name:   "quoted pattern only",
			chunks: []string{"\"foo working-tree-encoding=UTF-16\" ordinary-attribute\n"},
		},
		{
			name:   "attribute after quoted pattern",
			chunks: []string{"\"foo bar\" working-tree-encoding=UTF-16\r\n"},
			want:   true,
		},
		{
			name:   "escaped quote in quoted pattern",
			chunks: []string{"\"foo \\\" bar\" working-tree-encoding=UTF-16\n"},
			want:   true,
		},
		{name: "malformed quoted pattern", chunks: []string{"\"foo bar"}, want: true},
		{name: "overlong line", chunks: []string{strings.Repeat("x", maxAttributeLineBytes+1)}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			detector := &workingTreeEncodingDetector{}
			for _, chunk := range test.chunks {
				if _, err := detector.Write([]byte(chunk)); err != nil {
					t.Fatal(err)
				}
			}
			if got := detector.rejects(); got != test.want {
				t.Fatalf("rejects = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMaterializationTreePathBudgets(t *testing.T) {
	t.Parallel()
	t.Run("path bytes before split", func(t *testing.T) {
		path := strings.Repeat("x", maxMaterializationPathBytes+1)
		if err := validateMaterializationTreePath(path); err == nil {
			t.Fatal("overlong tree path was accepted")
		}
	})

	t.Run("path depth before split", func(t *testing.T) {
		path := strings.Repeat("x/", maxMaterializationPathDepth) + "x"
		if err := validateMaterializationTreePath(path); err == nil {
			t.Fatal("overdeep tree path was accepted")
		}
	})

	t.Run("component byte boundary", func(t *testing.T) {
		for _, test := range []struct {
			name string
			path string
			want bool
		}{
			{name: "254 bytes", path: strings.Repeat("x", 254)},
			{name: "255 bytes", path: strings.Repeat("x", 255)},
			{name: "256 bytes", path: strings.Repeat("x", 256), want: true},
			{name: "nested 256 bytes", path: "dir/" + strings.Repeat("x", 256), want: true},
			{name: "256 utf8 bytes", path: strings.Repeat("é", 128), want: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				err := validateMaterializationTreePath(test.path)
				if got := errors.Is(err, ErrMaterializationRefused); got != test.want {
					t.Fatalf("validateMaterializationTreePath(%q) = %v, refusal %v, want %v", test.path, err, got, test.want)
				}
				if !test.want && err != nil {
					t.Fatalf("validateMaterializationTreePath(%q) = %v, want nil", test.path, err)
				}
				if test.want && !errors.Is(err, ErrGitTransport) {
					t.Fatalf("validateMaterializationTreePath(%q) = %v, want transport class", test.path, err)
				}
			})
		}
	})

	t.Run("implicit directories count", func(t *testing.T) {
		directories := map[string]struct{}{`.git`: {}, ".": {}}
		if err := accountMaterializationPath("shared/one", 0, directories, 5); err != nil {
			t.Fatalf("first path within budget: %v", err)
		}
		if err := accountMaterializationPath("shared/two", 1, directories, 5); err != nil {
			t.Fatalf("shared ancestor was counted twice: %v", err)
		}
		if err := accountMaterializationPath("unique/three", 2, directories, 5); err == nil {
			t.Fatal("unique implicit directory exceeded the entry budget without refusal")
		}
	})
}

func TestRejectMaterializationFilesystemCollisions(t *testing.T) {
	t.Parallel()
	entry := materializationTreeEntry{mode: "100644", oid: strings.Repeat("a", 40), size: 1}
	for _, test := range []struct {
		name  string
		paths []string
		want  bool
	}{
		{name: "case-folded leaves", paths: []string{"README", "readme"}, want: true},
		{name: "normalization-folded leaves", paths: []string{"café", "cafe\u0301"}, want: true},
		{name: "case-folded implicit directory", paths: []string{"Dir/a", "dir/b"}, want: true},
		{name: "folded file and directory", paths: []string{"docs", "Docs/readme"}, want: true},
		{name: "distinct paths", paths: []string{"Dir/a", "other/b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries := make(map[string]materializationTreeEntry, len(test.paths))
			for _, treePath := range test.paths {
				entries[treePath] = entry
			}
			err := rejectMaterializationFilesystemCollisions(entries)
			if got := errors.Is(err, ErrMaterializationRefused); got != test.want {
				t.Fatalf("collision refusal = %v, want %v (error %v)", got, test.want, err)
			}
			if test.want && !errors.Is(err, ErrGitTransport) {
				t.Fatalf("collision error = %v, want transport class", err)
			}
			if !test.want && err != nil {
				t.Fatalf("distinct paths refused: %v", err)
			}
		})
	}

	t.Run("deep paths remain bounded", func(t *testing.T) {
		entries := make(map[string]materializationTreeEntry, 390)
		for i := range 390 {
			components := make([]string, maxMaterializationPathDepth)
			components[0] = fmt.Sprintf("root-%03d", i)
			for j := 1; j < len(components); j++ {
				components[j] = "x"
			}
			entries[strings.Join(components, "/")] = entry
		}
		if err := rejectMaterializationFilesystemCollisions(entries); err != nil {
			t.Fatalf("distinct deep paths refused: %v", err)
		}
	})
}

func TestMaterializationVerificationUsesRootedPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck // test-owned verification root

	components := make([]string, maxMaterializationPathDepth)
	for i := range components {
		components[i] = strings.Repeat("x", 15)
	}
	treePath := strings.Join(components, "/")
	if len(treePath) != maxMaterializationPathBytes-1 {
		t.Fatalf("fixture path has %d bytes, want %d", len(treePath), maxMaterializationPathBytes-1)
	}
	if err := root.MkdirAll(filepath.FromSlash(path.Dir(treePath)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(filepath.FromSlash(treePath), []byte("rooted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := materializationTreeEntry{mode: "100644"}
	if _, err := materializedObjectID(root, treePath, entry.mode); err != nil {
		t.Fatalf("verify near-limit rooted path: %v", err)
	}
	if err := walkMaterializationStrays(root, map[string]materializationTreeEntry{treePath: entry}); err != nil {
		t.Fatalf("walk near-limit rooted path: %v", err)
	}

	t.Run("raw non-UTF-8 path", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("reference Linux raw-filename behavior")
		}
		rawDir := string([]byte{'c', 'a', 'f', 0xe9})
		rawPath := rawDir + "/raw.txt"
		if err := root.Mkdir(rawDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(rawPath, []byte("raw\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		entries := map[string]materializationTreeEntry{treePath: entry, rawPath: entry}
		if err := walkMaterializationStrays(root, entries); err != nil {
			t.Fatalf("walk raw non-UTF-8 path: %v", err)
		}
	})
}

func TestMaterializationTreePathGitAliases(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		path string
		want bool
	}{
		{name: "plain dotgit", path: ".git/config", want: true},
		{name: "case variant", path: ".GIT/config", want: true},
		{name: "nested dotgit", path: "a/.git/config", want: true},
		{name: "ntfs short name", path: "git~1/config", want: true},
		{name: "ntfs trailing dot", path: ".git./config", want: true},
		{name: "ntfs trailing space", path: ".git /config", want: true},
		{name: "ntfs alternate separator", path: `a\b/config`, want: true},
		{name: "hfs zero width non-joiner", path: ".g\u200cit/config", want: true},
		{name: "hfs rtl override", path: "\u202e.git/config", want: true},
		{name: "hfs bom", path: ".git\ufeff/config", want: true},
		{name: "ntfs unnamed stream", path: ".git::$DATA/config", want: true},
		{name: "ntfs named stream", path: ".git:config/config", want: true},
		{name: "dotgit prefix", path: ".gitx/config"},
		{name: "dotgit suffix", path: "x.git/config"},
		{name: "gitignore", path: ".gitignore"},
		{name: "short-name lookalike", path: "agit~1/config"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateMaterializationTreePath(test.path)
			if got := errors.Is(err, ErrMaterializationRefused); got != test.want {
				t.Fatalf("validateMaterializationTreePath(%q) = %v, refusal %v, want %v", test.path, err, got, test.want)
			}
			if !test.want && err != nil {
				t.Fatalf("validateMaterializationTreePath(%q) = %v, want nil", test.path, err)
			}
			if test.want && !errors.Is(err, ErrGitTransport) {
				t.Fatalf("validateMaterializationTreePath(%q) = %v, want transport class", test.path, err)
			}
		})
	}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		body := ""
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path) //nolint:gosec // test-owned fixture tree
			if err != nil {
				return err
			}
			body = string(data)
		case info.Mode()&os.ModeSymlink != 0:
			body, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		snapshot[filepath.ToSlash(rel)] = fmt.Sprintf("%s\x00%s", info.Mode(), body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func snapshotWorktree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := snapshotTree(t, root)
	for path := range snapshot {
		if path == ".git" || strings.HasPrefix(path, ".git/") {
			delete(snapshot, path)
		}
	}
	return snapshot
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	if after := snapshotTree(t, root); !reflect.DeepEqual(after, before) {
		t.Errorf("refused materialization changed %s", root)
	}
}
