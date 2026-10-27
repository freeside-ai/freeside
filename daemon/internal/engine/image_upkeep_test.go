package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	upkeepRecipe      = `{"commands":[["npm","test"]],"capture":"none"}`
	upkeepPackageJSON = `{"name":"app","dependencies":{"left":"^1.0.0"}}`
	upkeepHeadJSON    = `{"name":"app","dependencies":{"left":"^1.0.0","right":"^2.0.0"}}`
	upkeepRegistrySet = `["registry.npmjs.org"]`
)

var (
	upkeepIntegrity = "sha512-" + strings.Repeat("A", 86) + "=="
	upkeepLeft      = `"node_modules/left":` + upkeepPinned("left", "1.0.0", "https")
	upkeepLock      = upkeepLockfile(upkeepPackageJSON, upkeepLeft)
	// upkeepAddedLock adds a pinned package from the declared registry.
	upkeepAddedLock = upkeepLockfile(upkeepHeadJSON, upkeepLeft,
		`"node_modules/right":`+upkeepPinned("right", "2.0.0", "https"))
	// upkeepUnpinnedLock adds the same package over plain http.
	upkeepUnpinnedLock = upkeepLockfile(upkeepHeadJSON, upkeepLeft,
		`"node_modules/right":`+upkeepPinned("right", "2.0.0", "http"))
	// upkeepStaleDigest stands for the preparation sources of an earlier
	// binary: what a daemon upgrade leaves behind.
	upkeepStaleDigest = domain.Digest("sha256:" + strings.Repeat("6", 64))
)

func upkeepPinned(name, version, scheme string) string {
	return `{"version":"` + version + `","resolved":"` + scheme + `://registry.npmjs.org/` + name + `/-/` +
		name + `-` + version + `.tgz","integrity":"` + upkeepIntegrity + `"}`
}

func upkeepLockfile(root string, members ...string) string {
	return `{"lockfileVersion":3,"packages":{"":` + root + "," + strings.Join(members, ",") + `}}`
}

func upkeepSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// upkeepRepo is the repository's default branch, which the fake checkout
// clones.
type upkeepRepo struct {
	t   *testing.T
	dir string
}

func newUpkeepRepo(t *testing.T) upkeepRepo {
	t.Helper()
	repo := upkeepRepo{t: t, dir: t.TempDir()}
	repo.git("init", "-q", "-b", "main", "--object-format=sha1")
	return repo
}

func (r upkeepRepo) git(args ...string) string {
	r.t.Helper()
	return runUpkeepGit(r.t, r.dir, args...)
}

func runUpkeepGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // G204: fixed test git invocation
	cmd.Dir = dir
	cmd.Env = append(gitenv.Scrubbed(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// commit applies changes and returns the new commit.
func (r upkeepRepo) commit(changes map[string]string) string {
	r.t.Helper()
	for path, content := range changes {
		full := filepath.Join(r.dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "change")
	return r.git("rev-parse", "HEAD")
}

func (r upkeepRepo) npmBase() string {
	return r.commit(map[string]string{
		"package.json": upkeepPackageJSON, "package-lock.json": upkeepLock,
		verify.DefaultRecipePath: upkeepRecipe, "src/index.js": "export {};\n",
	})
}

// upkeepImage is an npm image of the test repository built at commit.
func upkeepImage(t *testing.T, commit string, edit func(*domain.ProjectImageInput)) domain.ProjectImage {
	t.Helper()
	input := domain.ProjectImageInput{
		Repository: "owner/repo", RepositoryID: 42, CommitSHA: commit,
		RecipeDigest:       verify.RecipeDigest([]byte(upkeepRecipe)),
		PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef:       domain.ImageRef("ghcr.io/owner/base@sha256:" + strings.Repeat("c", 64)),
		ImageRef:           domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("d", 64)),
		Environment: &domain.ProjectImageEnvironment{
			PackageJSONSHA256: upkeepSHA256(upkeepPackageJSON), PackageLockSHA256: upkeepSHA256(upkeepLock),
			PreparationDigest: upkeepStaleDigest,
		},
	}
	if edit != nil {
		edit(&input)
	}
	image, err := domain.NewProjectImage(input)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// upkeepBuilder records an image for the requested commit from the
// manifests the checkout it was handed holds, as the real builder does.
type upkeepBuilder struct {
	st       *store.Store
	requests []projectimage.Request
	fail     error
	stall    bool
	// during runs inside the build, before the image is recorded.
	during func()
	// prepared, when set, is the preparation digest the record names
	// instead of the current one: a builder whose record disagrees with
	// the build it was asked for.
	prepared domain.Digest
	// goModule builds a Go image of the root module instead of an npm one.
	goModule bool
}

func (b *upkeepBuilder) Build(ctx context.Context, request projectimage.Request) (domain.ProjectImage, error) {
	b.requests = append(b.requests, request)
	if b.during != nil {
		b.during()
	}
	if b.fail != nil {
		return domain.ProjectImage{}, b.fail
	}
	if b.stall {
		<-ctx.Done()
		return domain.ProjectImage{}, ctx.Err()
	}
	layout := domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM}
	if b.goModule {
		layout.Toolchain = domain.ProjectToolchainGo
	}
	inputs, err := projectimage.ObserveBaseInputs(ctx, "git", request.SourceDir, request.CommitSHA, layout)
	if err != nil {
		return domain.ProjectImage{}, err
	}
	sum := sha256.Sum256([]byte(request.CommitSHA))
	prepared := projectimage.PreparationDigest()
	if b.prepared != "" {
		prepared = b.prepared
	}
	environment := &domain.ProjectImageEnvironment{
		PackageJSONSHA256: inputs.PackageJSONSHA256, PackageLockSHA256: inputs.PackageLockSHA256,
		PreparationDigest: prepared,
	}
	if b.goModule {
		environment = &domain.ProjectImageEnvironment{
			Toolchain: domain.ProjectToolchainGo, GoModSHA256: inputs.GoModSHA256, GoSumSHA256: inputs.GoSumSHA256,
			PreparationDigest: prepared,
		}
	}
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: request.Repository, RepositoryID: request.RepositoryID,
		CommitSHA: request.CommitSHA, RecipeDigest: verify.RecipeDigest(request.Recipe),
		PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef:       request.BaseImageRef,
		ImageRef: domain.ImageRef(fmt.Sprintf("%s/%s@sha256:%s",
			request.Registry, request.ImageName, hex.EncodeToString(sum[:]))),
		Environment: environment,
	})
	if err != nil {
		return domain.ProjectImage{}, err
	}
	return image, b.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordProjectImage(ctx, image)
	})
}

type upkeepRoom struct {
	recipe []byte
	err    error
}

func (upkeepRoom) Run(context.Context, string, []string) (verify.StepResult, error) {
	return verify.StepResult{}, errors.New("the recipe-read room runs nothing")
}

func (r upkeepRoom) ReadRecipe(context.Context) ([]byte, error) { return r.recipe, r.err }

type upkeepHarness struct {
	t       *testing.T
	st      *store.Store
	repo    upkeepRepo
	builder *upkeepBuilder
	upkeep  *ImageUpkeep
	// faults is every Health call, "" for a verdict.
	faults []string
	// checkouts counts fetches; beforeCheckout runs at the start of each.
	checkouts      int
	beforeCheckout func()
}

func newUpkeepHarness(t *testing.T, st *store.Store, repo upkeepRepo, registrySet string) *upkeepHarness {
	t.Helper()
	h := &upkeepHarness{t: t, st: st, repo: repo, builder: &upkeepBuilder{st: st}}
	var keys []domain.PolicyKey
	if registrySet != "" {
		keys = []domain.PolicyKey{{Key: domain.RegistrySetPolicyKey, Value: registrySet}}
	}
	policy := domain.ResolvedPolicy{Keys: keys}
	h.upkeep = &ImageUpkeep{
		Store: st, RepositoryID: 42,
		Rebuild: &ProjectImageRebuild{
			Builder: h.builder, Inputs: projectimage.BuildInputs{BaseBuildRef: "freeside-agent-claude:local"},
		},
		ReadTip: func(context.Context) (string, error) {
			return runUpkeepGit(t, repo.dir, "rev-parse", "main"), nil
		},
		Checkout: func(_ context.Context, tipSHA, dir string) (int64, error) {
			h.checkouts++
			if h.beforeCheckout != nil {
				h.beforeCheckout()
			}
			runUpkeepGit(t, filepath.Dir(dir), "clone", "-q", "--no-hardlinks", repo.dir, dir)
			runUpkeepGit(t, dir, "checkout", "-q", "--detach", tipSHA)
			return 42, nil
		},
		NewRoom: func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error) {
			return upkeepRoom{recipe: []byte(upkeepRecipe)}, nil
		},
		Policy: policy, PreparationDigest: projectimage.PreparationDigest(),
		WorkDir: t.TempDir(),
		Health: func(_ context.Context, fault string) error {
			h.faults = append(h.faults, fault)
			return nil
		},
		Now:    func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) },
		Logger: slog.New(slog.DiscardHandler),
	}
	return h
}

func openUpkeepStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seed records image and points the repository at it.
func (h *upkeepHarness) seed(image domain.ProjectImage) {
	h.t.Helper()
	if err := h.st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		if err := tx.RecordProjectImage(context.Background(), image); err != nil {
			return err
		}
		return tx.AdvanceAdmittedImagePointer(context.Background(), domain.AdmittedImagePointer{
			RepositoryID: image.RepositoryID, Repository: image.Repository, Harness: domain.HarnessClientClaudeCode,
			ImageID: image.ID, ImageRef: image.ImageRef,
			AdvancedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *upkeepHarness) pointer() domain.ProjectImage {
	h.t.Helper()
	var image domain.ProjectImage
	if err := h.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		_, image, _, err = tx.GetAdmittedImagePointer(context.Background(), 42, domain.HarnessClientClaudeCode)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return image
}

func (h *upkeepHarness) refusals() []domain.ImageUpkeepRefusal {
	h.t.Helper()
	var refusals []domain.ImageUpkeepRefusal
	if err := h.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		refusals, err = tx.ListImageUpkeepRefusals(context.Background(), 42)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return refusals
}

func (h *upkeepHarness) observeTip(tipSHA string) {
	h.t.Helper()
	if err := h.st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordRepositoryTipObservation(context.Background(), domain.RepositoryTipObservation{
			RepositoryID: 42, Repository: "owner/repo", Branch: "main", TipSHA: tipSHA,
			ObservedAt: time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *upkeepHarness) upkeepTip() string {
	h.t.Helper()
	var observation domain.RepositoryTipObservation
	if err := h.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		observation, _, err = tx.GetRepositoryTipObservation(context.Background(), 42)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return observation.UpkeepTipSHA
}

// A daemon upgrade changes the preparation digest. Whether the image was
// built at the tip or at an older commit with the same manifests, the start
// pass rebuilds it at the tip and advances the pointer with no host fault,
// records the tip it ran at, and the next pass finds the image current.
func TestImageUpkeepRebuildsAnImageAnUpgradeLeftBehind(t *testing.T) {
	t.Parallel()
	for _, olderBuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("built at an older commit %v", olderBuild), func(t *testing.T) {
			t.Parallel()
			repo := newUpkeepRepo(t)
			built := repo.npmBase()
			tip := built
			if olderBuild {
				tip = repo.commit(map[string]string{"src/index.js": "export const a = 1;\n"})
			}
			h := newUpkeepHarness(t, openUpkeepStore(t), repo, "")
			h.seed(upkeepImage(t, built, nil))
			h.observeTip(tip)

			if err := h.upkeep.RunAll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(h.builder.requests) != 1 || h.builder.requests[0].CommitSHA != tip {
				t.Fatalf("builds = %+v, want one at the tip %s", h.builder.requests, tip)
			}
			advanced := h.pointer()
			if advanced.CommitSHA != tip || advanced.Environment.PreparationDigest != projectimage.PreparationDigest() {
				t.Fatalf("pointer names %+v, want the image rebuilt at the tip", advanced)
			}
			if len(h.refusals()) != 0 || len(h.faults) != 1 || h.faults[0] != "" {
				t.Fatalf("refusals = %v, health = %q; want none and a cleared item", h.refusals(), h.faults)
			}
			if got := h.upkeepTip(); got != tip {
				t.Fatalf("upkeep tip = %q, want %s", got, tip)
			}

			if err := h.upkeep.RunAll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(h.builder.requests) != 1 || h.pointer().ID != advanced.ID {
				t.Fatal("a pass over a current image built again or moved the pointer")
			}
		})
	}
}

// A dependency change inside the project's policy is rebuilt for and
// admitted, as the publication lane's rebuild is.
func TestImageUpkeepAdvancesForAWithinPolicyDependencyChange(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	h := newUpkeepHarness(t, openUpkeepStore(t), repo, upkeepRegistrySet)
	h.seed(upkeepImage(t, built, func(input *domain.ProjectImageInput) {
		input.Environment.PreparationDigest = projectimage.PreparationDigest()
	}))
	tip := repo.commit(map[string]string{"package.json": upkeepHeadJSON, "package-lock.json": upkeepAddedLock})

	if err := h.upkeep.TipAdvanced(t.Context(), 42, tip); err != nil {
		t.Fatal(err)
	}
	advanced := h.pointer()
	if advanced.CommitSHA != tip || advanced.Environment.PackageLockSHA256 != upkeepSHA256(upkeepAddedLock) {
		t.Fatalf("pointer names %+v, want the image rebuilt at the tip", advanced)
	}
	if len(h.refusals()) != 0 {
		t.Fatalf("refusals = %v, want none", h.refusals())
	}
	if err := h.upkeep.TipAdvanced(t.Context(), 43, tip); !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("a tip of another repository = %v, want refused", err)
	}
}

// A Go module change pinned in go.sum is rebuilt for too, once the project
// declares the module proxy. The go.sum lines are go mod tidy output.
func TestImageUpkeepAdvancesForAWithinPolicyGoModuleChange(t *testing.T) {
	t.Parallel()
	const (
		goMod = "module example.test/app\n\ngo 1.26\n\nrequire github.com/google/uuid v1.6.0\n"
		goSum = "github.com/google/uuid v1.6.0 h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=\n" +
			"github.com/google/uuid v1.6.0/go.mod h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo=\n"
		headGoMod = goMod + "\nrequire golang.org/x/text v0.21.0\n"
		headGoSum = goSum + "golang.org/x/text v0.21.0 h1:zyQAAkrwaneQ066sspRyJaG9VNi/YJ1NfzcGB3hZ/qo=\n" +
			"golang.org/x/text v0.21.0/go.mod h1:4IBbMaMmOPCJ8SecivzSH54+73PCFmPWxNTLm+vZkEQ=\n"
	)
	repo := newUpkeepRepo(t)
	built := repo.commit(map[string]string{
		"go.mod": goMod, "go.sum": goSum, verify.DefaultRecipePath: upkeepRecipe, "main.go": "package main\n",
	})
	h := newUpkeepHarness(t, openUpkeepStore(t), repo, `["proxy.golang.org"]`)
	h.builder.goModule = true
	h.seed(upkeepImage(t, built, func(input *domain.ProjectImageInput) {
		input.Environment = &domain.ProjectImageEnvironment{
			Toolchain: domain.ProjectToolchainGo, GoModSHA256: upkeepSHA256(goMod), GoSumSHA256: upkeepSHA256(goSum),
			PreparationDigest: projectimage.PreparationDigest(),
		}
	}))
	tip := repo.commit(map[string]string{"go.mod": headGoMod, "go.sum": headGoSum})

	if err := h.upkeep.TipAdvanced(t.Context(), 42, tip); err != nil {
		t.Fatal(err)
	}
	advanced := h.pointer()
	if advanced.CommitSHA != tip || advanced.Environment.GoSumSHA256 != upkeepSHA256(headGoSum) {
		t.Fatalf("pointer names %+v, want the image rebuilt at the tip", advanced)
	}
	if len(h.refusals()) != 0 {
		t.Fatalf("refusals = %v, want none", h.refusals())
	}
}

// Each refusal is classified and recorded once: a rerun at the same tip
// converges on the first record. Only a host fault files the health item.
func TestImageUpkeepRecordsEachRefusalClassOnce(t *testing.T) {
	t.Parallel()
	const otherRecipe = `{"commands":[["true"]],"capture":"none"}`
	const goMod = "module example.test/app\n\ngo 1.22\n"
	const goSum = "example.test/dep v1.0.0 h1:" + "AAAA" + "=\n"
	tests := []struct {
		name string
		// registrySet defaults to the declared one; "none" declares none.
		registrySet string
		// base replaces the npm files the image is built from; image edits
		// the seeded npm image; tip changes the base.
		base  map[string]string
		image func(*domain.ProjectImageInput)
		tip   map[string]string
		// harness adjusts the job before it runs.
		harness   func(*upkeepHarness)
		want      domain.ImageUpkeepRefusalClass
		wantFault bool
	}{
		{
			name: "a recipe change is policy",
			tip:  map[string]string{verify.DefaultRecipePath: otherRecipe},
			want: domain.ImageUpkeepRefusalPolicy,
		},
		{
			name: "a recipe change with an unpinned source is repair",
			tip: map[string]string{
				verify.DefaultRecipePath: otherRecipe,
				"package.json":           upkeepHeadJSON, "package-lock.json": upkeepUnpinnedLock,
			},
			want: domain.ImageUpkeepRefusalRepair,
		},
		{
			name:        "a lockfile change with no registry set is policy",
			registrySet: "none",
			tip:         map[string]string{"package.json": upkeepHeadJSON, "package-lock.json": upkeepAddedLock},
			want:        domain.ImageUpkeepRefusalPolicy,
		},
		{
			name: "an npm input no image supports",
			tip:  map[string]string{".npmrc": "registry=https://registry.npmjs.org/\n"},
			want: domain.ImageUpkeepRefusalUnsupportedNPMInput,
		},
		{
			name: "a Go input no image supports",
			base: map[string]string{
				"go.mod": goMod, "go.sum": goSum, verify.DefaultRecipePath: upkeepRecipe, "main.go": "package main\n",
			},
			image: func(input *domain.ProjectImageInput) {
				input.Environment = &domain.ProjectImageEnvironment{
					Toolchain:   domain.ProjectToolchainGo,
					GoModSHA256: upkeepSHA256(goMod), GoSumSHA256: upkeepSHA256(goSum),
					PreparationDigest: upkeepStaleDigest,
				}
			},
			tip:  map[string]string{"vendor/modules.txt": "# example.test/dep v1.0.0\n"},
			want: domain.ImageUpkeepRefusalGoModuleDelta,
		},
		{
			name:    "a proof failure is repair",
			tip:     map[string]string{"src/index.js": "export const a = 1;\n"},
			harness: func(h *upkeepHarness) { h.builder.fail = projectimage.ErrProofFailed },
			want:    domain.ImageUpkeepRefusalRepair,
		},
		{
			name:      "a builder fault",
			tip:       map[string]string{"src/index.js": "export const a = 1;\n"},
			harness:   func(h *upkeepHarness) { h.builder.fail = errors.New("the container runtime is gone") },
			want:      domain.ImageUpkeepRefusalHostFault,
			wantFault: true,
		},
		{
			name:      "no rebuild configured",
			tip:       map[string]string{"src/index.js": "export const a = 1;\n"},
			harness:   func(h *upkeepHarness) { h.upkeep.Rebuild = nil },
			want:      domain.ImageUpkeepRefusalHostFault,
			wantFault: true,
		},
		{
			name: "a recipe-read room that cannot start",
			tip:  map[string]string{"src/index.js": "export const a = 1;\n"},
			harness: func(h *upkeepHarness) {
				h.upkeep.NewRoom = func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error) {
					return nil, errors.New("no container runtime")
				}
			},
			want:      domain.ImageUpkeepRefusalHostFault,
			wantFault: true,
		},
		{
			name: "a recipe the image record does not name",
			tip:  map[string]string{"src/index.js": "export const a = 1;\n"},
			harness: func(h *upkeepHarness) {
				h.upkeep.NewRoom = func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error) {
					return upkeepRoom{recipe: []byte(upkeepRecipe + "# altered\n")}, nil
				}
			},
			want:      domain.ImageUpkeepRefusalHostFault,
			wantFault: true,
		},
		{
			name:      "a rebuilt record that disagrees with its build",
			tip:       map[string]string{"src/index.js": "export const a = 1;\n"},
			harness:   func(h *upkeepHarness) { h.builder.prepared = upkeepStaleDigest },
			want:      domain.ImageUpkeepRefusalHostFault,
			wantFault: true,
		},
		{
			// A seeded image built off the branch leaves no build commit to
			// measure a change from.
			name: "an image whose build commit the branch lacks",
			image: func(input *domain.ProjectImageInput) {
				input.CommitSHA = strings.Repeat("e", 40)
				input.Environment = nil
			},
			tip:       map[string]string{"src/index.js": "export const a = 1;\n"},
			want:      domain.ImageUpkeepRefusalHostFault,
			wantFault: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo := newUpkeepRepo(t)
			var built string
			if test.base != nil {
				built = repo.commit(test.base)
			} else {
				built = repo.npmBase()
			}
			registrySet := upkeepRegistrySet
			if test.registrySet == "none" {
				registrySet = ""
			}
			h := newUpkeepHarness(t, openUpkeepStore(t), repo, registrySet)
			image := upkeepImage(t, built, test.image)
			h.seed(image)
			if test.harness != nil {
				test.harness(h)
			}
			tip := repo.commit(test.tip)
			h.observeTip(tip)

			for range 2 {
				if err := h.upkeep.RunAll(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			refusals := h.refusals()
			if len(refusals) != 1 {
				t.Fatalf("refusals = %+v, want one", refusals)
			}
			refusal := refusals[0]
			if refusal.Class != test.want || refusal.TipSHA != tip || refusal.MeasuredFromImageID != image.ID {
				t.Fatalf("refusal = %+v, want %s at %s measured from %s", refusal, test.want, tip, image.ID)
			}
			if h.pointer().ID != image.ID {
				t.Fatal("a refused pass moved the pointer")
			}
			if len(h.faults) != 2 || (h.faults[1] != "") != test.wantFault {
				t.Fatalf("health = %q, want a host fault %v", h.faults, test.wantFault)
			}
			// A host fault is retried on the next trigger, so it does not
			// count as having run at the tip.
			wantTip := tip
			if test.wantFault {
				wantTip = ""
			}
			if got := h.upkeepTip(); got != wantTip {
				t.Fatalf("upkeep tip = %q, want %q", got, wantTip)
			}

			// The schedule takes a settled verdict as standing instead of
			// fetching and building the same tip again; a host fault is
			// measured again.
			checkouts, builds := h.checkouts, len(h.builder.requests)
			if err := h.upkeep.RunScheduled(t.Context()); err != nil {
				t.Fatal(err)
			}
			if remeasured := h.checkouts != checkouts; remeasured != test.wantFault {
				t.Fatalf("scheduled pass measured the tip again: %v, want %v", remeasured, test.wantFault)
			}
			if !test.wantFault && len(h.builder.requests) != builds {
				t.Fatalf("scheduled pass built %d times, want none", len(h.builder.requests)-builds)
			}
			if len(h.refusals()) != 1 || len(h.faults) != 3 || (h.faults[2] != "") != test.wantFault {
				t.Fatalf("after the scheduled pass: refusals = %+v, health = %q", h.refusals(), h.faults)
			}
		})
	}
}

// A pass whose pointer another pass moved, before it took the gate or
// during its build, neither builds over it nor moves it back.
func TestImageUpkeepYieldsToAPointerAnotherPassMoved(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before the gate", "during the build"} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()
			repo := newUpkeepRepo(t)
			built := repo.npmBase()
			h := newUpkeepHarness(t, openUpkeepStore(t), repo, "")
			h.seed(upkeepImage(t, built, nil))
			other := upkeepImage(t, built, func(input *domain.ProjectImageInput) {
				input.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("e", 64))
			})
			move := func() { h.seed(other) }
			if when == "before the gate" {
				h.beforeCheckout = move
			} else {
				h.builder.during = move
			}
			if err := h.upkeep.RunAll(t.Context()); err != nil {
				t.Fatal(err)
			}
			wantBuilds := 0
			if when == "during the build" {
				wantBuilds = 1
			}
			if len(h.builder.requests) != wantBuilds || h.pointer().ID != other.ID {
				t.Fatalf("builds = %d, pointer = %s; want %d and the other pass's image",
					len(h.builder.requests), h.pointer().ImageRef, wantBuilds)
			}
			if len(h.faults) != 0 || len(h.refusals()) != 0 {
				t.Fatalf("health = %q, refusals = %v; want no verdict", h.faults, h.refusals())
			}
		})
	}
}

// Upkeep and a publication rebuild share one builder. While upkeep builds,
// the lane finds the builder busy and moves on; afterwards it reuses the
// image upkeep built when that serves its commit, without building.
func TestImageUpkeepAndPublicationNeverBuildTogether(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	st := openUpkeepStore(t)
	h := newUpkeepHarness(t, st, repo, "")
	pointed := upkeepImage(t, built, nil)
	h.seed(pointed)
	tip := repo.commit(map[string]string{"src/index.js": "export const a = 1;\n"})
	head, err := projectimage.ObserveBaseInputs(t.Context(), "git", repo.dir, tip,
		domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM})
	if err != nil {
		t.Fatal(err)
	}
	lane := &productionPublicationWorkflow{
		store: st, rebuild: h.upkeep.Rebuild, preparationDigest: projectimage.PreparationDigest(),
	}
	publish := func() (domain.ProjectImage, error) {
		image, refusal, err := lane.rebuildUnderGate(t.Context(), productionPublicationTask{},
			productionBinding{image: pointed}, repo.dir, tip, head)
		if refusal != nil {
			t.Fatalf("publication refused: %+v", refusal)
		}
		return image, err
	}
	var busy error
	h.builder.during = func() { _, busy = publish() }

	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(busy, errProjectImageBuilderBusy) || !errors.Is(busy, errProductionRetryable) {
		t.Fatalf("publication during an upkeep build = %v, want a retryable busy builder", busy)
	}
	h.builder.during = nil
	reused, err := publish()
	if err != nil {
		t.Fatal(err)
	}
	if len(h.builder.requests) != 1 || reused.ID != h.pointer().ID {
		t.Fatalf("builds = %d, publication bound %s; want upkeep's one image reused",
			len(h.builder.requests), reused.ImageRef)
	}
}

// The hour bound ends a build that never finishes as a host fault, which the
// next trigger retries.
func TestImageUpkeepBoundsAStuckBuildAsAHostFault(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	st := openUpkeepStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newUpkeepHarness(t, st, repo, "")
		h.seed(upkeepImage(t, built, nil))
		h.builder.stall = true
		h.upkeep.Now = time.Now
		start := time.Now()
		if err := h.upkeep.RunAll(t.Context()); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != defaultProjectImageRebuildTimeout {
			t.Fatalf("the build ran %s, want the %s bound", elapsed, defaultProjectImageRebuildTimeout)
		}
		refusals := h.refusals()
		if len(refusals) != 1 || refusals[0].Class != domain.ImageUpkeepRefusalHostFault ||
			len(h.faults) != 1 || !strings.Contains(h.faults[0], "did not finish within 1h0m0s") {
			t.Fatalf("refusals = %+v, health = %q; want one host fault naming the bound", refusals, h.faults)
		}
	})
}

// A pass cancelled during its build reached no verdict on the tip, so it
// records no refusal and leaves the health item alone.
func TestImageUpkeepRecordsNothingForACancelledBuild(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	h := newUpkeepHarness(t, openUpkeepStore(t), repo, "")
	image := upkeepImage(t, built, nil)
	h.seed(image)
	ctx, cancel := context.WithCancel(t.Context())
	h.builder.stall = true
	h.builder.during = cancel
	if err := h.upkeep.RunAll(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAll = %v, want the cancellation", err)
	}
	if refusals := h.refusals(); len(refusals) != 0 || len(h.faults) != 0 || h.pointer().ID != image.ID {
		t.Fatalf("refusals = %+v, health = %q; want nothing recorded", refusals, h.faults)
	}
}

// A recorded image that serves the tip is reused only when its build commit
// is on the branch: the next pass measures the tip from that commit, and one
// built off the branch (at a pull request's head) leaves none to measure
// from.
func TestImageUpkeepReusesOnlyAnImageBuiltOnTheBranch(t *testing.T) {
	t.Parallel()
	for _, onBranch := range []bool{true, false} {
		t.Run(fmt.Sprintf("on the branch %v", onBranch), func(t *testing.T) {
			t.Parallel()
			repo := newUpkeepRepo(t)
			built := repo.npmBase()
			tip := repo.commit(map[string]string{"src/index.js": "export const a = 1;\n"})
			h := newUpkeepHarness(t, openUpkeepStore(t), repo, "")
			h.seed(upkeepImage(t, built, nil))
			recordedAt := strings.Repeat("e", 40)
			if onBranch {
				recordedAt = tip
			}
			recorded := upkeepImage(t, recordedAt, func(input *domain.ProjectImageInput) {
				input.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("f", 64))
				input.Environment.PreparationDigest = projectimage.PreparationDigest()
			})
			if err := h.st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
				return tx.RecordProjectImage(t.Context(), recorded)
			}); err != nil {
				t.Fatal(err)
			}

			if err := h.upkeep.RunAll(t.Context()); err != nil {
				t.Fatal(err)
			}
			pointed := h.pointer()
			if onBranch {
				if len(h.builder.requests) != 0 || pointed.ID != recorded.ID {
					t.Fatalf("builds = %d, pointer = %s; want the recorded image reused",
						len(h.builder.requests), pointed.ID)
				}
				return
			}
			if len(h.builder.requests) != 1 || pointed.ID == recorded.ID || pointed.CommitSHA != tip {
				t.Fatalf("builds = %d, pointer = %+v; want a rebuild at the tip", len(h.builder.requests), pointed)
			}
		})
	}
}

// PendingResolution says what an attempt whose base no image serves waits
// on: a rebuild until this process settles a verdict at the observed tip,
// the card or task that answers a settled refusal, and nothing once the
// verdict stands with no answer.
func TestImageUpkeepPendingResolution(t *testing.T) {
	t.Parallel()
	const otherRecipe = `{"commands":[["true"]],"capture":"none"}`
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	h := newUpkeepHarness(t, openUpkeepStore(t), repo, upkeepRegistrySet)
	h.seed(upkeepImage(t, built, func(input *domain.ProjectImageInput) {
		input.Environment.PreparationDigest = projectimage.PreparationDigest()
	}))
	pending := func() *domain.ImageUpkeepHold {
		t.Helper()
		hold, err := h.upkeep.PendingResolution(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if hold != nil {
			if err := hold.Validate(); err != nil {
				t.Fatalf("PendingResolution returned an invalid hold: %v", err)
			}
		}
		return hold
	}
	if hold := pending(); hold == nil || hold.Pending != domain.ImageUpkeepPendingRebuild {
		t.Fatalf("before any pass = %+v, want a rebuild", hold)
	}

	h.observeTip(built)
	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if hold := pending(); hold != nil {
		t.Fatalf("with the image current at the tip = %+v, want nothing pending", hold)
	}
	h.observeTip(repo.commit(map[string]string{"src/index.js": "export const a = 1;\n"}))
	if hold := pending(); hold == nil || hold.Pending != domain.ImageUpkeepPendingRebuild {
		t.Fatalf("after the tip moved = %+v, want a rebuild", hold)
	}

	refused := repo.commit(map[string]string{verify.DefaultRecipePath: otherRecipe})
	h.observeTip(refused)
	if err := h.upkeep.TipAdvanced(t.Context(), 42, refused); err != nil {
		t.Fatal(err)
	}
	refusals := h.refusals()
	if len(refusals) != 1 {
		t.Fatalf("refusals = %+v, want one", refusals)
	}
	if hold := pending(); hold != nil {
		t.Fatalf("a refusal nothing answers = %+v, want nothing pending", hold)
	}
	h.upkeep.MaintenancePending = func(_ context.Context, refusal domain.ImageUpkeepRefusal) (domain.TaskID, bool, error) {
		return "task-1", refusal.ID == refusals[0].ID, nil
	}
	if hold := pending(); hold == nil || hold.Pending != domain.ImageUpkeepPendingMaintenanceTask ||
		*hold.RefusalRecordID != refusals[0].ID || *hold.MaintenanceTaskID != "task-1" {
		t.Fatalf("a refusal a maintenance task answers = %+v", hold)
	}
	h.upkeep.AdoptionPending = func(_ context.Context, refusal domain.ImageUpkeepRefusal) (domain.ItemID, bool, error) {
		return "card-1", refusal.ID == refusals[0].ID, nil
	}
	if hold := pending(); hold == nil || hold.Pending != domain.ImageUpkeepPendingAdoptionProposal ||
		*hold.RefusalRecordID != refusals[0].ID || *hold.ProposalItemID != "card-1" {
		t.Fatalf("a refusal an adoption card answers = %+v", hold)
	}
}

// A host fault is not a verdict: an attempt keeps waiting on a rebuild.
func TestImageUpkeepPendingResolutionTreatsAHostFaultAsARebuild(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	h := newUpkeepHarness(t, openUpkeepStore(t), repo, "")
	h.seed(upkeepImage(t, built, nil))
	h.upkeep.Rebuild = nil
	h.observeTip(built)
	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if refusals := h.refusals(); len(refusals) != 1 || refusals[0].Class != domain.ImageUpkeepRefusalHostFault {
		t.Fatalf("refusals = %+v, want one host fault", refusals)
	}
	hold, err := h.upkeep.PendingResolution(t.Context())
	if err != nil || hold == nil || hold.Pending != domain.ImageUpkeepPendingRebuild {
		t.Fatalf("after a host fault = %+v, %v; want a rebuild", hold, err)
	}
}
