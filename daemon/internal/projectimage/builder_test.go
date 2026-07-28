package projectimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	testCommit      = "6ab4e3dff2be53f74bde9b8b3150290775152f9f"
	testBaseDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testImageDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRecipe      = `{"commands":[["npm","run","lint"],["npm","run","typecheck"],["npm","test"]],"capture":"none"}`
)

type fakeSource struct {
	fetches int
	copies  int
}

func (f *fakeSource) Fetch(_ context.Context, repository, commit, destination string) error {
	f.fetches++
	if repository != "freeasinbird/gh-imgup" || commit != testCommit {
		return errors.New("unexpected source identity")
	}
	return os.MkdirAll(destination, 0o700)
}

func (f *fakeSource) Copy(_ context.Context, _ string, commit, destination string) error {
	f.copies++
	if commit != testCommit {
		return errors.New("unexpected copied commit")
	}
	return writeNPMFixture(destination)
}

type fakeResolver struct {
	calls int
	errs  []error
}

func (f *fakeResolver) Verify(context.Context, string, int64) error {
	f.calls++
	if f.calls <= len(f.errs) {
		return f.errs[f.calls-1]
	}
	return nil
}

func writeNPMFixture(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "package.json"), []byte(`{"scripts":{}}`), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "package-lock.json"), []byte(`{"lockfileVersion":3}`), 0o600)
}

type fakeBackend struct {
	baseDigest     string
	localDigest    string
	published      domain.ImageRef
	allowlist      []string
	provenance     []string
	pins           [][3]string
	releases       []string
	runs           []runSpec
	builds         []buildSpec
	publishes      []publishSpec
	negativeOutput []byte
	negativeErr    error
	failRecipe     string
	allowlistErr   error
	provenanceErr  error
	removeLocalErr error
	containerfile  string
	cleanups       int
	discards       int
	leaseReleases  int
	publicationLog []string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		baseDigest: testBaseDigest, localDigest: testImageDigest,
		published:      domain.ImageRef("127.0.0.1:5100/freeside-project-freeasinbird-gh-imgup@" + testImageDigest),
		negativeOutput: []byte("npm error request to https://registry.npmjs.org/pkg failed: ENOTFOUND"),
		negativeErr:    errors.New("exit status 1"),
	}
}

func (f *fakeBackend) ImageDigest(_ context.Context, ref string) (string, error) {
	if ref == "freeside-agent-claude:local" {
		return f.baseDigest, nil
	}
	return f.localDigest, nil
}

func (f *fakeBackend) PinBase(_ context.Context, sourceRef, privateRef, digest string) error {
	f.pins = append(f.pins, [3]string{sourceRef, privateRef, digest})
	return nil
}

func (f *fakeBackend) RemoveImage(_ context.Context, ref string) error {
	f.releases = append(f.releases, ref)
	if f.removeLocalErr != nil && len(f.builds) > 0 && ref == f.builds[0].LocalRef {
		return f.removeLocalErr
	}
	return nil
}

func (f *fakeBackend) Build(_ context.Context, spec buildSpec) error {
	f.builds = append(f.builds, spec)
	body, err := os.ReadFile(filepath.Join(spec.ContextDir, "Containerfile"))
	if err != nil {
		return err
	}
	f.containerfile = string(body)
	return nil
}

func (f *fakeBackend) CheckAllowlist(_ context.Context, ref string) error {
	f.allowlist = append(f.allowlist, ref)
	return f.allowlistErr
}

func (f *fakeBackend) CheckProvenance(_ context.Context, ref string, _ provenanceSpec) error {
	f.provenance = append(f.provenance, ref)
	return f.provenanceErr
}

func (f *fakeBackend) Run(_ context.Context, spec runSpec) (runResult, error) {
	spec.Argv = append([]string{}, spec.Argv...)
	f.runs = append(f.runs, spec)
	if spec.MaskCache {
		exitCode := 1
		if f.negativeErr == nil {
			exitCode = 0
		}
		return runResult{Output: f.negativeOutput, ExitCode: exitCode}, nil
	}
	if len(spec.Argv) > 0 && spec.Argv[0] == f.failRecipe {
		return runResult{Output: []byte("recipe failed"), ExitCode: 1}, nil
	}
	return runResult{Output: []byte("ok")}, nil
}

func (f *fakeBackend) Publish(_ context.Context, spec publishSpec) (publication, error) {
	f.publishes = append(f.publishes, spec)
	return publication{
		Ref: f.published,
		cleanup: func(context.Context) error {
			f.cleanups++
			f.publicationLog = append(f.publicationLog, "cleanup")
			return nil
		},
		discard: func(context.Context) error {
			f.discards++
			f.publicationLog = append(f.publicationLog, "discard")
			return nil
		},
		release: func() error {
			f.leaseReleases++
			f.publicationLog = append(f.publicationLog, "release")
			return nil
		},
	}, nil
}

func validRequest() Request {
	return Request{
		Repository: "freeasinbird/gh-imgup", RepositoryID: 1278475858,
		CommitSHA: testCommit, Recipe: []byte(testRecipe),
		BaseImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent-claude@" + testBaseDigest),
		BaseBuildRef:      "freeside-agent-claude:local",
		LocalRegistryPort: 5100,
	}
}

func TestBuildBindsExactInputsAndProvesEveryFreshWorkspace(t *testing.T) {
	source := &fakeSource{}
	backend := newFakeBackend()
	builder := newBuilder(source, backend, t.TempDir())
	records := 0
	builder.record = func(_ context.Context, image domain.ProjectImage) error {
		records++
		backend.publicationLog = append(backend.publicationLog, "record")
		if backend.leaseReleases != 0 {
			return errors.New("publication lease released before durable recording")
		}
		if image.ID == "" {
			return errors.New("empty recorded image")
		}
		return nil
	}
	got, err := builder.Build(t.Context(), validRequest())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.Repository != "freeasinbird/gh-imgup" || got.RepositoryID != 1278475858 ||
		got.CommitSHA != testCommit || got.BaseImageRef != validRequest().BaseImageRef ||
		got.ImageRef != backend.published {
		t.Fatalf("result = %+v", got)
	}
	if source.fetches != 1 || source.copies != 5 {
		t.Fatalf("source fetch/copies = %d/%d, want 1/5", source.fetches, source.copies)
	}
	if len(backend.builds) != 1 || backend.builds[0].BaseDigest != testBaseDigest ||
		backend.builds[0].CommitSHA != testCommit ||
		backend.builds[0].BaseRef == validRequest().BaseBuildRef {
		t.Fatalf("build spec = %+v", backend.builds)
	}
	if len(backend.pins) != 1 ||
		backend.pins[0][0] != validRequest().BaseBuildRef ||
		backend.pins[0][1] != backend.builds[0].BaseRef ||
		backend.pins[0][2] != testBaseDigest ||
		!slices.Equal(backend.releases, []string{
			backend.builds[0].BaseRef,
			backend.builds[0].LocalRef,
		}) {
		t.Fatalf("image pins/removals = %v/%v", backend.pins, backend.releases)
	}
	if len(backend.allowlist) != 2 ||
		backend.allowlist[0] == string(backend.published) ||
		backend.allowlist[1] != string(backend.published) {
		t.Fatalf("allowlist refs = %v, want local then published", backend.allowlist)
	}
	if len(backend.provenance) != 1 || backend.provenance[0] == string(backend.published) {
		t.Fatalf("provenance refs = %v, want the local built image", backend.provenance)
	}
	if backend.leaseReleases != 1 {
		t.Fatalf("publication lease releases = %d, want 1", backend.leaseReleases)
	}
	if !slices.Equal(backend.publicationLog, []string{"record", "release"}) {
		t.Fatalf("publication lifecycle = %v, want record then release", backend.publicationLog)
	}
	if len(backend.runs) != 8 {
		t.Fatalf("runs = %d, want preflight + preparation/recipe per command + negative", len(backend.runs))
	}
	wantRecipe := [][]string{
		{"npm", "run", "lint"}, {"npm", "run", "typecheck"}, {"npm", "test"},
	}
	for index, want := range wantRecipe {
		preparation := backend.runs[1+index*2]
		recipe := backend.runs[2+index*2]
		if !slices.Equal(preparation.Argv, []string{PreparationPath}) {
			t.Errorf("preparation %d argv = %q", index, preparation.Argv)
		}
		if !slices.Equal(recipe.Argv, want) {
			t.Errorf("recipe %d argv = %q, want exact %q", index, recipe.Argv, want)
		}
		if preparation.Workspace != recipe.Workspace {
			t.Errorf("command %d preparation and recipe used different workspaces", index)
		}
		if index > 0 && recipe.Workspace == backend.runs[index*2].Workspace {
			t.Errorf("recipe %d reused the previous fresh workspace", index)
		}
	}
	if !backend.runs[7].MaskCache ||
		!slices.Equal(backend.runs[7].Argv, []string{"npm", "ci", "--ignore-scripts"}) {
		t.Fatalf("negative run = %+v", backend.runs[7])
	}
	if len(backend.publishes) != 1 || backend.publishes[0].Digest != testImageDigest {
		t.Fatalf("publish = %+v", backend.publishes)
	}
	if records != 1 || backend.cleanups != 0 || backend.discards != 0 {
		t.Fatalf("records/cleanups/discards = %d/%d/%d, want durable retain",
			records, backend.cleanups, backend.discards)
	}

	for _, forbidden := range []string{"\nENV ", "\nWORKDIR ", "\nENTRYPOINT ", "\nCMD ", "\nUSER ", "\nVOLUME "} {
		if strings.Contains(backend.containerfile, forbidden) {
			t.Errorf("generated Containerfile carries forbidden metadata %q", strings.TrimSpace(forbidden))
		}
	}
}

func TestBuildRejectsInvalidInputsBeforeSourceAccess(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"bad repository", func(r *Request) { r.Repository = "../repo" }},
		{"missing repository id", func(r *Request) { r.RepositoryID = 0 }},
		{"short commit", func(r *Request) { r.CommitSHA = "abc" }},
		{"uppercase commit", func(r *Request) { r.CommitSHA = strings.ToUpper(r.CommitSHA) }},
		{"malformed recipe", func(r *Request) { r.Recipe = []byte(`{"commands":[]}`) }},
		{"tagged base", func(r *Request) { r.BaseImageRef = "example.test/base:latest" }},
		{"missing build ref", func(r *Request) { r.BaseBuildRef = "" }},
		{"option-shaped build ref", func(r *Request) { r.BaseBuildRef = "--help" }},
		{"digest build ref", func(r *Request) { r.BaseBuildRef = "example.test/base@sha256:abc" }},
		{"two registries", func(r *Request) { r.Registry = "registry.example.test" }},
		{"no registry", func(r *Request) { r.LocalRegistryPort = 0 }},
		{"privileged local port", func(r *Request) { r.LocalRegistryPort = 80 }},
		{"scheme in registry", func(r *Request) {
			r.LocalRegistryPort = 0
			r.Registry = "https://registry.example.test"
		}},
		{"option-shaped registry", func(r *Request) {
			r.LocalRegistryPort = 0
			r.Registry = "--help"
		}},
		{"absolute registry path", func(r *Request) {
			r.LocalRegistryPort = 0
			r.Registry = "/registry.example.test"
		}},
		{"bad image name", func(r *Request) { r.ImageName = "UPPER" }},
		{"bad tag", func(r *Request) { r.RefTag = "-bad" }},
		{"option-shaped dns", func(r *Request) { r.DNS = []string{"--bad"} }},
		{"credentialed build proxy", func(r *Request) {
			r.BuildProxy = "http://user:secret@proxy.example.test:8080"
		}},
		{"non-http build proxy", func(r *Request) {
			r.BuildProxy = "https://proxy.example.test:8080"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := validRequest()
			tc.mutate(&request)
			source := &fakeSource{}
			_, err := newBuilder(source, newFakeBackend(), t.TempDir()).Build(t.Context(), request)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Build = %v, want ErrInvalidRequest", err)
			}
			if source.fetches != 0 {
				t.Fatal("invalid input reached repository fetch")
			}
		})
	}
}

func TestBuildRefutesBaseAndProofFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeBackend)
	}{
		{"base digest mismatch", func(b *fakeBackend) { b.baseDigest = "sha256:" + strings.Repeat("c", 64) }},
		{"allowlist rejection", func(b *fakeBackend) { b.allowlistErr = errors.New("shape mismatch") }},
		{"provenance rejection", func(b *fakeBackend) {
			b.provenanceErr = errors.New("label mismatch")
		}},
		{"recipe failure", func(b *fakeBackend) { b.failRecipe = "npm" }},
		{"negative cache probe succeeds", func(b *fakeBackend) { b.negativeErr = nil }},
		{"negative misses network class", func(b *fakeBackend) {
			b.negativeOutput = []byte("permission denied")
		}},
		{"published digest mismatch", func(b *fakeBackend) {
			b.published = domain.ImageRef("example.test/project@sha256:" + strings.Repeat("c", 64))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := newFakeBackend()
			tc.mutate(backend)
			_, err := newBuilder(&fakeSource{}, backend, t.TempDir()).Build(t.Context(), validRequest())
			if !errors.Is(err, ErrProofFailed) {
				t.Fatalf("Build = %v, want ErrProofFailed", err)
			}
		})
	}
}

func TestBuildReverifiesRepositoryIdentityAfterFetch(t *testing.T) {
	resolver := &fakeResolver{}
	builder := newBuilder(&fakeSource{}, newFakeBackend(), t.TempDir())
	builder.resolver = resolver
	if _, err := builder.Build(t.Context(), validRequest()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resolver.calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (pre- and post-fetch)", resolver.calls)
	}
}

func TestBuildRefutesRepositoryIdentityDriftAfterFetch(t *testing.T) {
	drift := fmt.Errorf("repository freeasinbird/gh-imgup resolved to id 999: %w", ErrInvalidRequest)
	resolver := &fakeResolver{errs: []error{nil, drift}}
	source := &fakeSource{}
	backend := newFakeBackend()
	builder := newBuilder(source, backend, t.TempDir())
	builder.resolver = resolver
	_, err := builder.Build(t.Context(), validRequest())
	if !errors.Is(err, ErrProofFailed) {
		t.Fatalf("Build = %v, want ErrProofFailed", err)
	}
	if source.fetches != 1 || source.copies != 0 {
		t.Fatalf("source fetch/copies = %d/%d, want 1/0 (fail before materialization)", source.fetches, source.copies)
	}
	if len(backend.builds) != 0 || len(backend.publishes) != 0 {
		t.Fatalf("backend builds/publishes = %d/%d, want 0/0", len(backend.builds), len(backend.publishes))
	}
}

func TestBuildCleansPublicationWhenDurableRecordingFails(t *testing.T) {
	backend := newFakeBackend()
	builder := newBuilder(&fakeSource{}, backend, t.TempDir())
	builder.record = func(context.Context, domain.ProjectImage) error {
		backend.publicationLog = append(backend.publicationLog, "record")
		return errors.New("database unavailable")
	}
	got, err := builder.Build(t.Context(), validRequest())
	if err == nil || got.ID != "" ||
		!strings.Contains(err.Error(), "record project image") {
		t.Fatalf("Build = %+v, %v; want recording failure", got, err)
	}
	if backend.cleanups != 1 || backend.discards != 1 {
		t.Fatalf("publication cleanups/discards = %d/%d, want 1/1",
			backend.cleanups, backend.discards)
	}
	if backend.leaseReleases != 1 {
		t.Fatalf("publication lease releases = %d, want 1", backend.leaseReleases)
	}
	if !slices.Equal(backend.publicationLog, []string{"record", "discard", "cleanup", "release"}) {
		t.Fatalf(
			"publication lifecycle = %v, want record then discard then cleanup then release",
			backend.publicationLog,
		)
	}
	if !slices.Contains(backend.releases, backend.builds[0].LocalRef) {
		t.Fatalf("image removals = %v, want local build removed", backend.releases)
	}
}

func TestBuildDiscardsSeededPublicationWhenPublishedProofFails(t *testing.T) {
	backend := newFakeBackend()
	backend.published = domain.ImageRef("example.test/project@sha256:" + strings.Repeat("c", 64))
	got, err := newBuilder(&fakeSource{}, backend, t.TempDir()).Build(t.Context(), validRequest())
	if !errors.Is(err, ErrProofFailed) || got.ID != "" {
		t.Fatalf("Build = %+v, %v; want published-proof failure", got, err)
	}
	if backend.discards != 1 || backend.cleanups != 1 {
		t.Fatalf("publication discards/cleanups = %d/%d, want 1/1",
			backend.discards, backend.cleanups)
	}
	if !slices.Equal(backend.publicationLog, []string{"discard", "cleanup", "release"}) {
		t.Fatalf(
			"publication lifecycle = %v, want discard then cleanup then release",
			backend.publicationLog,
		)
	}
}

func TestBuildThreadsRecordedRefLookupIntoPublish(t *testing.T) {
	backend := newFakeBackend()
	builder := newBuilder(&fakeSource{}, backend, t.TempDir())
	lookups := 0
	builder.lookupRecordedRef = func(context.Context, string) (bool, error) {
		lookups++
		return true, nil
	}
	if _, err := builder.Build(t.Context(), validRequest()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(backend.publishes) != 1 || backend.publishes[0].RefRecorded == nil {
		t.Fatal("Build did not hand the recorded-reference guard to Publish")
	}
	if recorded, err := backend.publishes[0].RefRecorded(t.Context(), "ref"); err != nil ||
		!recorded || lookups != 1 {
		t.Fatalf("RefRecorded = %v, %v (lookups %d), want the builder's lookup", recorded, err, lookups)
	}
}

func TestBuildRemovesLocalImageAfterPostBuildFailure(t *testing.T) {
	backend := newFakeBackend()
	backend.provenanceErr = errors.New("label mismatch")

	_, err := newBuilder(&fakeSource{}, backend, t.TempDir()).Build(t.Context(), validRequest())
	if !errors.Is(err, ErrProofFailed) {
		t.Fatalf("Build = %v, want ErrProofFailed", err)
	}
	if !slices.Contains(backend.releases, backend.builds[0].LocalRef) {
		t.Fatalf("image removals = %v, want local build removed", backend.releases)
	}
}

func TestBuildFailsWhenLocalImageRemovalFails(t *testing.T) {
	backend := newFakeBackend()
	backend.removeLocalErr = errors.New("runtime unavailable")

	got, err := newBuilder(&fakeSource{}, backend, t.TempDir()).Build(t.Context(), validRequest())
	if err == nil || got.ID != "" || !strings.Contains(err.Error(), "remove local project image") {
		t.Fatalf("Build = %+v, %v; want local image cleanup failure", got, err)
	}
}

func TestBuildContextRejectsSymlinkedManifest(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "real.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.json", filepath.Join(source, "package.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "package-lock.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := createBuildContext(filepath.Join(t.TempDir(), "context"), source, validRequest(), "sha256:recipe")
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("createBuildContext = %v, want non-regular refusal", err)
	}
}

func TestBuildContextRejectsAlternateNPMInputs(t *testing.T) {
	for _, name := range []string{"npm-shrinkwrap.json", ".npmrc"} {
		t.Run(name, func(t *testing.T) {
			source := t.TempDir()
			if err := writeNPMFixture(source); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, name), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := createBuildContext(
				filepath.Join(t.TempDir(), "context"),
				source,
				validRequest(),
				"sha256:recipe",
			)
			if err == nil || !strings.Contains(err.Error(), "unsupported npm input "+name) {
				t.Fatalf("createBuildContext = %v, want alternate npm input refusal", err)
			}
		})
	}
}

func TestBuildContextPreparationBindsNPMInputs(t *testing.T) {
	source := t.TempDir()
	if err := writeNPMFixture(source); err != nil {
		t.Fatal(err)
	}
	contextDir := filepath.Join(t.TempDir(), "context")
	if err := createBuildContext(
		contextDir, source, validRequest(), "sha256:recipe",
	); err != nil {
		t.Fatal(err)
	}
	preparation, err := os.ReadFile(filepath.Join(contextDir, "prepare")) //nolint:gosec // fixed file under test-owned build context
	if err != nil {
		t.Fatal(err)
	}
	containerfile, err := os.ReadFile(filepath.Join(contextDir, "Containerfile")) //nolint:gosec // fixed file under test-owned build context
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range []string{"package.json", "package-lock.json"} {
		if !strings.Contains(
			string(preparation),
			"cmp -s "+manifest+" /opt/freeside/project-seed/"+manifest,
		) {
			t.Errorf("preparation does not bind baked %s", manifest)
		}
	}
	for _, unsupported := range []string{"npm-shrinkwrap.json", ".npmrc"} {
		if !strings.Contains(string(preparation), "[ -e "+unsupported+" ]") {
			t.Errorf("preparation does not reject %s", unsupported)
		}
	}
	for _, fixedConfig := range []string{
		"NPM_CONFIG_GLOBALCONFIG=/usr/local/etc/npmrc",
		"NPM_CONFIG_USERCONFIG=/dev/null",
	} {
		if !strings.Contains(string(preparation), fixedConfig) {
			t.Errorf("preparation does not fix %s", fixedConfig)
		}
		if !strings.Contains(string(containerfile), fixedConfig) {
			t.Errorf("cache warming does not fix %s", fixedConfig)
		}
	}
	if !strings.Contains(string(preparation), "exec npm ci --ignore-scripts") {
		t.Error("preparation permits candidate lifecycle scripts")
	}
}
