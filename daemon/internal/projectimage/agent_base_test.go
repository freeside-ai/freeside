package projectimage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	testClaudeVersion = "2.1.319"
	pinnedDebian      = "docker.io/library/debian:trixie-slim@sha256:" +
		"020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd"
)

// agentBaseContainerfile is the shape of images/agent-claude/Containerfile:
// pins declared before the first FROM, a build stage, and a final stage.
func agentBaseContainerfile(version string) string {
	return "ARG BASE_IMAGE=" + pinnedDebian + "\n" +
		"ARG CLAUDE_CODE_VERSION=" + version + "\n" +
		"FROM ${BASE_IMAGE} AS build\nARG CLAUDE_CODE_VERSION\nRUN true\n" +
		"FROM $BASE_IMAGE\nCOPY --from=build /opt /opt\n"
}

// The checked-in source must pass the rule upkeep builds it under, or no
// daemon could ever build it.
func TestCheckedInAgentBaseContainerfileIsBuildable(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(domain.AgentBaseSourceDir), "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkAgentBaseContainerfile(content); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAgentBaseContainerfile(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"argument default":  agentBaseContainerfile(testClaudeVersion),
		"literal image":     "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + "\n",
		"earlier stage":     "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + " AS one\nFROM one\n",
		"platform flag":     "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM --platform=linux/arm64 " + pinnedDebian + "\n",
		"comment and blank": "# a comment\n\nARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + "\n",
		"copy from a stage": "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + " AS one\nFROM " + pinnedDebian +
			"\nCOPY --from=one /a /a\nRUN --mount=type=cache,target=/c true\n",
		"mount a pinned image": "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian +
			"\nRUN --mount=type=bind,from=" + pinnedDebian + ",target=/m true\n",
		"continued run": "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian +
			"\nRUN apt-get update \\\n    # a comment the builder drops\n    && true\n",
	} {
		version, err := checkAgentBaseContainerfile([]byte(content))
		if err != nil || version != testClaudeVersion {
			t.Errorf("%s: version %q err %v, want %s", name, version, err, testClaudeVersion)
		}
	}
	for name, content := range map[string]string{
		"tag only":            "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM debian:trixie-slim\n",
		"argument tag only":   "ARG BASE_IMAGE=debian:trixie\nARG CLAUDE_CODE_VERSION=2.1.319\nFROM ${BASE_IMAGE}\n",
		"argument after FROM": "FROM " + pinnedDebian + " AS one\nARG LATER=" + pinnedDebian + "\nFROM ${LATER}\nARG CLAUDE_CODE_VERSION=2.1.319\n",
		"undeclared argument": "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM ${BASE_IMAGE}\n",
		"mixed argument":      "ARG REGISTRY=docker.io\nARG CLAUDE_CODE_VERSION=2.1.319\nFROM ${REGISTRY}/library/debian@sha256:" + strings.Repeat("a", 64) + "\n",
		"no image":            "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM --platform=linux/arm64\n",
		"no version":          "FROM " + pinnedDebian + "\n",
		"version range":       "ARG CLAUDE_CODE_VERSION=latest\nFROM " + pinnedDebian + "\n",
		"two versions":        "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + "\nARG CLAUDE_CODE_VERSION=2.1.320\n",
		"syntax frontend":     "# syntax=docker/dockerfile:1\nARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + "\n",
		"copy from a tag":     "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + "\nCOPY --from=busybox:latest /bin/sh /sh\n",
		"mount a tag":         "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + "\nRUN --mount=type=bind,from=busybox,target=/m true\n",
		"escape directive":    "# escape=`\nARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + "\n",
		"continued copy":      "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + "\nCOPY \\\n    --from=alpine:latest /bin/sh /x\n",
		"continued mount":     "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM " + pinnedDebian + "\nRUN \\\n    --mount=type=bind,from=alpine:latest,target=/m true\n",
	} {
		if _, err := checkAgentBaseContainerfile([]byte(content)); !errors.Is(err, ErrAgentBaseSource) {
			t.Errorf("%s: err %v, want ErrAgentBaseSource", name, err)
		}
	}
}

func TestReadAgentBaseSourceReadsTheTreeAndPinAtTheCommit(t *testing.T) {
	t.Parallel()
	repo := newBaseInputsRepo(t)
	containerfile := domain.AgentBaseSourceDir + "/Containerfile"
	empty := repo.commit(map[string]string{"README.md": "x"})
	pinned := repo.commit(map[string]string{containerfile: agentBaseContainerfile(testClaudeVersion)})
	tree := repo.git("rev-parse", pinned+":"+domain.AgentBaseSourceDir)
	unpinned := repo.commit(map[string]string{containerfile: "ARG CLAUDE_CODE_VERSION=2.1.320\nFROM debian\n"})

	source, err := ReadAgentBaseSource(t.Context(), "git", repo.dir, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if source.TreeSHA != tree || source.ClaudeVersion != testClaudeVersion {
		t.Fatalf("source = %+v, want tree %s and %s", source, tree, testClaudeVersion)
	}
	for name, commit := range map[string]string{"no directory": empty, "unpinned FROM": unpinned} {
		if _, err := ReadAgentBaseSource(t.Context(), "git", repo.dir, commit); !errors.Is(err, ErrAgentBaseSource) {
			t.Errorf("%s: err %v, want ErrAgentBaseSource", name, err)
		}
	}
}

// agentBaseFixture is a fake backend whose probes answer as a good base
// does, and a source whose copy writes the agent base directory.
func agentBaseFixture(t *testing.T, containerfile string) (*Builder, *fakeBackend) {
	t.Helper()
	backend := newFakeBackend()
	backend.published = domain.ImageRef("127.0.0.1:5100/" + AgentBaseImageName + "@" + testImageDigest)
	backend.runAnswer = func(spec runSpec) (runResult, bool) {
		if spec.Argv[0] == "claude" {
			return runResult{Output: []byte(testClaudeVersion + " (Claude Code)\n")}, true
		}
		return runResult{}, false
	}
	source := &fakeSource{fixture: func(destination string) error {
		dir := filepath.Join(destination, filepath.FromSlash(domain.AgentBaseSourceDir))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(containerfile), 0o600)
	}}
	return newBuilder(source, backend, t.TempDir()), backend
}

func agentBaseRequest() AgentBaseRequest {
	return AgentBaseRequest{
		SourceDir: "/checkout", CommitSHA: testCommit, ClaudeVersion: testClaudeVersion,
		Destination: domain.ImageRef("127.0.0.1:5100/freeside-project-freeasinbird-gh-imgup@" + testImageDigest),
	}
}

func TestBuildAgentBasePublishesBesideTheProjectImageAndTagsIt(t *testing.T) {
	t.Parallel()
	builder, backend := agentBaseFixture(t, agentBaseContainerfile(testClaudeVersion))
	base, err := builder.BuildAgentBase(t.Context(), agentBaseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if base.ImageRef != backend.published || base.ClaudeVersion != testClaudeVersion ||
		base.LocalTag != AgentBaseLocalTag(backend.published) {
		t.Fatalf("base = %+v", base)
	}
	if len(backend.builds) != 1 || !backend.builds[0].AgentBase ||
		!strings.HasSuffix(backend.builds[0].ContextDir, filepath.FromSlash(domain.AgentBaseSourceDir)) {
		t.Fatalf("builds = %+v", backend.builds)
	}
	if len(backend.publishes) != 1 || backend.publishes[0].ImageName != AgentBaseImageName ||
		backend.publishes[0].LocalRegistryPort != 5100 || backend.publishes[0].RefTag != "base-"+testCommit[:12] {
		t.Fatalf("publishes = %+v", backend.publishes)
	}
	localRef := backend.builds[0].LocalRef
	if len(backend.allowlist) != 2 || backend.allowlist[0] != localRef || backend.allowlist[1] != string(backend.published) {
		t.Fatalf("allowlist checks = %v, want the local and the published image", backend.allowlist)
	}
	if len(backend.pins) != 1 || backend.pins[0] != [3]string{localRef, base.LocalTag, testImageDigest} {
		t.Fatalf("pins = %v", backend.pins)
	}
	if backend.cleanups != 0 || backend.discards != 0 || backend.leaseReleases != 1 {
		t.Fatalf("publication cleanups %d discards %d releases %d", backend.cleanups, backend.discards, backend.leaseReleases)
	}
	if len(backend.releases) != 1 || backend.releases[0] != localRef {
		t.Fatalf("removed images = %v, want only the build reference", backend.releases)
	}
}

func TestBuildAgentBaseRefusesWhatItCannotProve(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		containerfile string
		mutate        func(*fakeBackend)
		want          error
		published     bool
	}{
		"another pin": {containerfile: agentBaseContainerfile("2.1.320"), want: ErrAgentBaseSource},
		"unpinned base": {
			containerfile: "ARG CLAUDE_CODE_VERSION=2.1.319\nFROM debian\n", want: ErrAgentBaseSource,
		},
		"another CLI version": {mutate: func(b *fakeBackend) {
			b.runAnswer = func(spec runSpec) (runResult, bool) {
				return runResult{Output: []byte("2.1.3190 (Claude Code)")}, spec.Argv[0] == "claude"
			}
		}, want: ErrProofFailed},
		"plugins left enabled": {mutate: func(b *fakeBackend) {
			previous := b.runAnswer
			b.runAnswer = func(spec runSpec) (runResult, bool) {
				if spec.Argv[0] == "setpriv" {
					return runResult{ExitCode: 1}, true
				}
				return previous(spec)
			}
		}, want: ErrProofFailed},
		"allowlist": {mutate: func(b *fakeBackend) { b.allowlistErr = errors.New("shape mismatch") }, want: ErrProofFailed},
		"published allowlist": {mutate: func(b *fakeBackend) {
			b.publishedAllowlistErr = errors.New("shape mismatch")
		}, want: ErrProofFailed, published: true},
		"published elsewhere": {mutate: func(b *fakeBackend) {
			b.published = domain.ImageRef("127.0.0.1:5100/other@" + testImageDigest)
		}, want: ErrProofFailed, published: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			containerfile := tc.containerfile
			if containerfile == "" {
				containerfile = agentBaseContainerfile(testClaudeVersion)
			}
			builder, backend := agentBaseFixture(t, containerfile)
			if tc.mutate != nil {
				tc.mutate(backend)
			}
			if _, err := builder.BuildAgentBase(t.Context(), agentBaseRequest()); !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
			if published := len(backend.publishes) != 0; published != tc.published {
				t.Fatalf("published = %v, want %v", published, tc.published)
			}
			if tc.published && backend.discards != 1 {
				t.Fatalf("a refused publication was discarded %d times, want once", backend.discards)
			}
			if len(backend.pins) != 0 {
				t.Fatalf("a refused base was tagged: %v", backend.pins)
			}
		})
	}
}
