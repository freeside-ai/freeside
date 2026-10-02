package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// maxAdoptPromptBytes bounds a prompt file adoption digests.
const maxAdoptPromptBytes = 4 << 20

// authAdoptDeps are the side-effecting collaborators a test replaces: the
// container observation of the Claude volume, and the clock.
type authAdoptDeps struct {
	observeVolume func(containerBin, exporterImage string) func(context.Context, string) (domain.Digest, error)
	now           func() time.Time
}

func productionAuthAdoptDeps() authAdoptDeps {
	return authAdoptDeps{
		observeVolume: func(bin, image string) func(context.Context, string) (domain.Digest, error) {
			return ward.ObserveSetupTokenVolume(ward.NewCLIRuntime(bin), image, nil)
		},
		now: func() time.Time { return time.Now().UTC() },
	}
}

// authAdoptConfig is the interim selection the daemon's flags carry, given
// again here because this command cannot read the daemon's flags.
type authAdoptConfig struct {
	DBPath          string
	ApprovedRecipes digestSetFlag

	AuthIdentityID, ReviewAuthIdentityID              string
	CostOwner, ReviewCostOwner, ShadowReviewCostOwner string

	ClaudeAccount, ContainerBin, ExporterImage string
	AuthStoreRoot                              string

	ClaudeRoute, CodexRoute, ReviewModel           string
	TermsBasisDate, PricingRevision, OfferNotAfter string
	// The daemon's three prompt-package files; the lineup lines record their
	// digests. The review prompt is code-owned and needs no file.
	PromptPackage, SpecificationPromptPackage, RemediationPromptPackage string
	PatchPath                                                           string
	// RetireUnadoptable names the one identity to retire when this run
	// reports it unadoptable.
	RetireUnadoptable string
}

// authAdoptStatus is what adoption did for one identity.
type authAdoptStatus string

const (
	authAdoptAdopted     authAdoptStatus = "adopted"
	authAdoptReused      authAdoptStatus = "reused"
	authAdoptUnadoptable authAdoptStatus = "unadoptable"
)

// authAdoptIdentity reports one identity's adoption.
type authAdoptIdentity struct {
	AuthIdentityID domain.AuthIdentityID     `json:"auth_identity_id"`
	Client         domain.HarnessClientKind  `json:"harness_client"`
	Status         authAdoptStatus           `json:"status"`
	EnrollmentID   domain.ClientEnrollmentID `json:"enrollment_id,omitempty"`
	Generation     int                       `json:"generation,omitempty"`
	// Reason says why an unadoptable identity cannot be adopted. The cutover
	// retires that identity; enroll a fresh one with auth add.
	Reason string `json:"reason,omitempty"`
	// Disabled reports an adopted identity that is stored disabled. Adoption
	// does not enable it, and agent resolution refuses it until it is enabled.
	Disabled bool `json:"disabled,omitempty"`
	// Retired reports that -retire-unadoptable disabled the identity, and
	// StoppedTasks the open tasks it owned, each now holding a Stop.
	Retired      bool            `json:"retired,omitempty"`
	StoppedTasks []domain.TaskID `json:"stopped_tasks,omitempty"`
}

// authAdoptReport is the command's result. Patch is where the tree patch
// went, empty when none was emitted.
type authAdoptReport struct {
	Identities     []authAdoptIdentity `json:"identities"`
	Patch          string              `json:"patch,omitempty"`
	LineupRevision domain.Digest       `json:"lineup_revision,omitempty"`
}

// runAuthAdoptCommand enrolls the flag-selected identities over the stores
// they already hold and emits the admitted-agent tree patch that selects
// them (plan §5.4). It writes no credential and changes no tree: a human
// reviews and commits the patch. It refuses a running daemon like auth add,
// because it takes each identity's mutation lease.
func runAuthAdoptCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer, deps authAdoptDeps,
) (err error) {
	cfg, err := parseAuthAdoptConfig(args, stderr, deps.now())
	if err != nil {
		return err
	}
	prompts, err := readAdoptPrompts(cfg)
	if err != nil {
		return err
	}
	notAfter, err := time.Parse(time.RFC3339, cfg.OfferNotAfter)
	if err != nil {
		return fmt.Errorf("-offer-not-after: %w", err)
	}
	lock, err := requireNoDaemon("auth adopt", cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	st, _, err := openStoreWithTopicKey(ctx, cfg.DBPath, store.Options{ApprovedRecipes: cfg.ApprovedRecipes})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	adapters, err := wardstore.New(st)
	if err != nil {
		return err
	}

	// Check every cost owner against the stored identities before recording
	// anything, so a mismatch on the second identity leaves the first alone.
	claudeID := domain.AuthIdentityID(cfg.AuthIdentityID)
	reviewID := domain.AuthIdentityID(cfg.ReviewAuthIdentityID)
	claudeIdentity, err := adoptableIdentity(ctx, adapters, claudeID, cfg.CostOwner)
	if err != nil {
		return err
	}
	var reviewIdentity domain.AuthIdentity
	if reviewID != claudeID {
		if reviewIdentity, err = adoptableIdentity(ctx, adapters, reviewID, cfg.ReviewCostOwner); err != nil {
			return err
		}
	}

	shared := func(identity domain.AuthIdentity, client domain.HarnessClientKind, route, owner string) ward.AuthAdoptionConfig {
		return ward.AuthAdoptionConfig{
			Identity:     identity,
			EnrollmentID: domain.ClientEnrollmentID(string(identity.ID) + "/" + string(client)),
			Route:        route, CostOwner: owner,
			Store: adapters.Adoption, AuthStoreLeaser: adapters.Leaser, Now: deps.now,
		}
	}
	// The patch owns stdout when it goes there, so it pipes into git apply;
	// the report then goes to stderr.
	reportOut := stdout
	if cfg.PatchPath == "-" {
		reportOut = stderr
	}
	var report authAdoptReport
	claude, claudeErr := ward.AdoptClaudeAuth(ctx, ward.ClaudeAuthAdoptionConfig{
		AuthAdoptionConfig: shared(claudeIdentity, domain.HarnessClientClaudeCode, cfg.ClaudeRoute, cfg.CostOwner),
		AccountBinding:     cfg.ClaudeAccount,
		ObserveVolume:      deps.observeVolume(cfg.ContainerBin, cfg.ExporterImage),
	})
	claudeEntry, err := adoptReportEntry(claudeIdentity, domain.HarnessClientClaudeCode, claude, claudeErr)
	if err != nil {
		return err
	}
	report.Identities = append(report.Identities, claudeEntry)
	if err := retireUnadoptable(ctx, st, cfg, &report.Identities[0], deps.now()); err != nil {
		return errors.Join(err, writeAdoptReport(reportOut, report))
	}

	// One identity under both flags is one adoption: it holds one store, and
	// that store is the Claude one, so the tree carries no review agent.
	var codexEnrollment *domain.ClientEnrollment
	if reviewID != claudeID {
		codex, codexErr := ward.AdoptCodexAuth(ctx, ward.CodexAuthAdoptionConfig{
			AuthAdoptionConfig: shared(reviewIdentity, domain.HarnessClientCodexCLI, cfg.CodexRoute, cfg.ReviewCostOwner),
			AuthStoreRoot:      cfg.AuthStoreRoot,
		})
		entry, err := adoptReportEntry(reviewIdentity, domain.HarnessClientCodexCLI, codex, codexErr)
		if err != nil {
			// The first identity's adoption is already recorded; say so
			// before failing, so the operator knows a rerun reuses it.
			return errors.Join(err, writeAdoptReport(reportOut, report))
		}
		report.Identities = append(report.Identities, entry)
		if err := retireUnadoptable(ctx, st, cfg, &report.Identities[1], deps.now()); err != nil {
			return errors.Join(err, writeAdoptReport(reportOut, report))
		}
		if codexErr == nil {
			codexEnrollment = &codex.Enrollment
		}
	}
	if codexEnrollment != nil {
		// With no review agent there is no reviewer line; the cutover's
		// startup check names the role when policy still asks review from ward.
		prompts[domain.RoleReviewer] = reviewPrompt()
	}
	if claudeErr != nil {
		// Every baseline writer role runs the Claude agent, so there is no
		// tree to emit without it.
		return errors.Join(
			writeAdoptReport(reportOut, report),
			fmt.Errorf("auth identity %s is unadoptable, so no tree patch was emitted", claudeID),
		)
	}

	tree, err := agentbaseline.Tree(agentbaseline.TreeInput{
		ClaudeEnrollment: claude.Enrollment, CodexEnrollment: codexEnrollment,
		ReviewModel: cfg.ReviewModel, TermsBasisDate: cfg.TermsBasisDate,
		PricingRevision: cfg.PricingRevision, OfferNotAfter: notAfter, Prompts: prompts,
	})
	if err != nil {
		return err
	}
	files, err := agenttree.Render(tree)
	if err != nil {
		return err
	}
	if report.LineupRevision, err = agenttree.Revision(files); err != nil {
		return err
	}
	patch, err := agenttree.Patch(agenttree.Files{}, files)
	if err != nil {
		return err
	}
	if cfg.PatchPath == "-" {
		report.Patch = "stdout"
		if _, err := stdout.Write(patch); err != nil {
			return fmt.Errorf("write tree patch: %w", err)
		}
		return writeAdoptReport(reportOut, report)
	}
	if err := os.WriteFile(cfg.PatchPath, patch, 0o600); err != nil {
		return fmt.Errorf("write tree patch: %w", err)
	}
	report.Patch = cfg.PatchPath
	return writeAdoptReport(reportOut, report)
}

func writeAdoptReport(out io.Writer, report authAdoptReport) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write adoption report: %w", err)
	}
	return nil
}

// retireUnadoptable retires the identity the operator named, when this run
// reported it unadoptable: it disables the identity and records a Stop for
// each open task it owns. The identity has no enrollment, so no agent can
// resolve to it and that work cannot continue through the lineup. Nothing
// else about the identity changes. An identity this run adopted is never
// retired: naming one is refused, so an omitted argument cannot cost an
// adoptable identity its work.
func retireUnadoptable(
	ctx context.Context, st *store.Store, cfg authAdoptConfig, entry *authAdoptIdentity, now time.Time,
) error {
	if cfg.RetireUnadoptable == "" || string(entry.AuthIdentityID) != cfg.RetireUnadoptable {
		return nil
	}
	if entry.Status != authAdoptUnadoptable {
		return fmt.Errorf("auth identity %s is %s, and -retire-unadoptable retires only an unadoptable identity",
			entry.AuthIdentityID, entry.Status)
	}
	// Disabled first: if a Stop then fails, the daemon holds admission on
	// the work that is left, and a rerun records the rest.
	err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		identity, err := tx.GetAuthIdentity(ctx, entry.AuthIdentityID)
		if err != nil || !identity.Enabled {
			return err
		}
		identity.Enabled = false
		return tx.RecordAuthIdentity(ctx, identity, now)
	})
	if err != nil {
		return fmt.Errorf("retire auth identity %s: %w", entry.AuthIdentityID, err)
	}
	entry.Retired = true
	entry.StoppedTasks, err = stopRetiredTasks(ctx, st, entry.AuthIdentityID, now)
	if err != nil {
		return fmt.Errorf("retire auth identity %s: %w", entry.AuthIdentityID, err)
	}
	return nil
}

// adoptableIdentity reads a flag-era identity and refuses a cost owner that
// disagrees with the one it already has.
func adoptableIdentity(
	ctx context.Context, adapters *wardstore.Adapters, id domain.AuthIdentityID, costOwner string,
) (domain.AuthIdentity, error) {
	identity, err := adapters.Leaser.GetIdentity(ctx, id)
	if err != nil {
		return domain.AuthIdentity{}, err
	}
	if identity.CostOwner != "" && identity.CostOwner != costOwner {
		return domain.AuthIdentity{}, fmt.Errorf("auth identity %s has cost owner %q, not %q",
			id, identity.CostOwner, costOwner)
	}
	return identity, nil
}

// adoptReportEntry turns one adoption's outcome into its report line. Only
// an unadoptable identity is a reportable outcome; any other failure aborts.
func adoptReportEntry(
	identity domain.AuthIdentity, client domain.HarnessClientKind, result ward.AuthAdoptionResult, err error,
) (authAdoptIdentity, error) {
	entry := authAdoptIdentity{AuthIdentityID: identity.ID, Client: client}
	switch {
	case errors.Is(err, ward.ErrUnadoptable):
		entry.Status, entry.Reason = authAdoptUnadoptable, err.Error()
		return entry, nil
	case err != nil:
		return authAdoptIdentity{}, err
	}
	entry.Status = authAdoptAdopted
	if result.Reused {
		entry.Status = authAdoptReused
	}
	entry.EnrollmentID, entry.Generation = result.Enrollment.ID, result.Generation.Ordinal
	entry.Disabled = !identity.Enabled
	return entry, nil
}

// readAdoptPrompts digests the daemon's prompt packages into the writer
// roles' lineup prompts: the role name, with the digest admission records for
// the package (the content address of its bytes).
func readAdoptPrompts(cfg authAdoptConfig) (map[domain.RoleName]agentbaseline.Prompt, error) {
	prompts := map[domain.RoleName]agentbaseline.Prompt{}
	for _, entry := range []struct {
		role       domain.RoleName
		flag, path string
	}{
		{domain.RoleSpecifier, "specification-prompt-package", cfg.SpecificationPromptPackage},
		{domain.RoleImplementer, "prompt-package", cfg.PromptPackage},
		{domain.RoleRemediator, "remediation-prompt-package", cfg.RemediationPromptPackage},
	} {
		body, err := readBoundedFile(entry.path, maxAdoptPromptBytes)
		if err != nil {
			return nil, fmt.Errorf("-%s: %w", entry.flag, err)
		}
		prompts[entry.role] = agentbaseline.Prompt{
			Name: string(entry.role), Digest: domain.Digest(contentaddr.Sum(body)),
		}
	}
	if cfg.ShadowReviewCostOwner != "" {
		prompts[domain.RoleShadowReviewer] = reviewPrompt()
	}
	return prompts, nil
}

// reviewPrompt is the code-owned production review prompt both review arms
// run.
func reviewPrompt() agentbaseline.Prompt {
	name, digest := ward.ProductionReviewPromptIdentity()
	return agentbaseline.Prompt{Name: name, Digest: digest}
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the operator names the prompt file to digest
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit || len(body) == 0 {
		return nil, fmt.Errorf("%s is empty or larger than %d bytes", path, limit)
	}
	return body, nil
}

func parseAuthAdoptConfig(args []string, stderr io.Writer, now time.Time) (authAdoptConfig, error) {
	cfg := authAdoptConfig{ApprovedRecipes: digestSetFlag{}}
	flags := flag.NewFlagSet("freesided auth adopt", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DBPath, "db", "", "SQLite database path (required)")
	flags.Var(&cfg.ApprovedRecipes, "approved-recipe", "approved verification-recipe digest (repeatable)")
	flags.StringVar(&cfg.AuthIdentityID, "auth-identity", "",
		"the flag-era -auth-identity: the Claude identity tasks ran under (required)")
	flags.StringVar(&cfg.ReviewAuthIdentityID, "review-auth-identity", "",
		"the flag-era -review-auth-identity: the Codex review identity (required)")
	flags.StringVar(&cfg.CostOwner, "cost-owner", "", "who pays for the implementation identity's usage (required)")
	flags.StringVar(&cfg.ReviewCostOwner, "review-cost-owner", "", "the flag-era -review-cost-owner (required)")
	flags.StringVar(&cfg.ShadowReviewCostOwner, "shadow-review-cost-owner", "",
		"the flag-era -shadow-review-cost-owner; set exactly while the shadow arm is on")
	flags.StringVar(&cfg.ClaudeAccount, "claude-account", "",
		"operator-attested subscription account the Claude setup token belongs to")
	flags.StringVar(&cfg.ContainerBin, "container-bin", "container", "Apple container CLI path")
	flags.StringVar(&cfg.ExporterImage, "exporter-image", "", "digest-pinned exporter image (required)")
	flags.StringVar(&cfg.AuthStoreRoot, "auth-store-root", "",
		"private root containing the review identity's live Codex auth store")
	flags.StringVar(&cfg.ClaudeRoute, "claude-route", agentbaseline.DefaultClaudeRoute, "route name for the Claude enrollment")
	flags.StringVar(&cfg.CodexRoute, "codex-route", agentbaseline.DefaultCodexRoute, "route name for the Codex enrollment")
	flags.StringVar(&cfg.ReviewModel, "review-model", "", "the daemon's -review-model, pinned by the review offer")
	flags.StringVar(&cfg.TermsBasisDate, "terms-basis-date", now.Format(time.DateOnly),
		"date of the operator's terms basis for both routes")
	flags.StringVar(&cfg.PricingRevision, "pricing-revision", now.Format("2006-01"), "pricing revision authored onto both offers")
	flags.StringVar(&cfg.OfferNotAfter, "offer-not-after", now.AddDate(1, 0, 0).Truncate(24*time.Hour).Format(time.RFC3339),
		"instant after which both offers stop admitting (RFC 3339)")
	flags.StringVar(&cfg.PromptPackage, "prompt-package", "", "the daemon's -prompt-package (required)")
	flags.StringVar(&cfg.SpecificationPromptPackage, "specification-prompt-package", "",
		"the daemon's -specification-prompt-package (required)")
	flags.StringVar(&cfg.RemediationPromptPackage, "remediation-prompt-package", "",
		"the daemon's -remediation-prompt-package (required)")
	flags.StringVar(&cfg.RetireUnadoptable, "retire-unadoptable", "",
		"identity to retire if this run reports it unadoptable: disable it and stop the open tasks it owns")
	flags.StringVar(&cfg.PatchPath, "patch", "-", "file to write the tree patch to, outside any checkout; - is stdout")
	if err := flags.Parse(args); err != nil {
		return authAdoptConfig{}, err
	}
	if flags.NArg() != 0 {
		return authAdoptConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	for _, required := range []struct{ name, value string }{
		{"db", cfg.DBPath},
		{"auth-identity", cfg.AuthIdentityID},
		{"review-auth-identity", cfg.ReviewAuthIdentityID},
		{"cost-owner", cfg.CostOwner},
		{"review-cost-owner", cfg.ReviewCostOwner},
		{"exporter-image", cfg.ExporterImage},
		{"prompt-package", cfg.PromptPackage},
		{"specification-prompt-package", cfg.SpecificationPromptPackage},
		{"remediation-prompt-package", cfg.RemediationPromptPackage},
	} {
		if required.value == "" {
			return authAdoptConfig{}, fmt.Errorf("-%s is required", required.name)
		}
	}
	oneIdentity := cfg.AuthIdentityID == cfg.ReviewAuthIdentityID
	if oneIdentity && cfg.ReviewCostOwner != cfg.CostOwner {
		return authAdoptConfig{}, errors.New(
			"-auth-identity and -review-auth-identity name one identity, so -review-cost-owner must equal -cost-owner")
	}
	if cfg.ShadowReviewCostOwner != "" && cfg.ShadowReviewCostOwner != cfg.CostOwner {
		return authAdoptConfig{}, errors.New(
			"-shadow-review-cost-owner must equal -cost-owner: the shadow arm runs under the implementation identity")
	}
	if !oneIdentity && (cfg.AuthStoreRoot == "" || cfg.ReviewModel == "") {
		return authAdoptConfig{}, errors.New("-auth-store-root and -review-model are required to adopt the review identity")
	}
	if cfg.RetireUnadoptable != "" && cfg.RetireUnadoptable != cfg.AuthIdentityID &&
		cfg.RetireUnadoptable != cfg.ReviewAuthIdentityID {
		return authAdoptConfig{}, errors.New(
			"-retire-unadoptable must name the -auth-identity or the -review-auth-identity")
	}
	if cfg.PatchPath != "-" {
		if err := refusePatchInsideCheckout(cfg.PatchPath); err != nil {
			return authAdoptConfig{}, err
		}
	}
	return cfg, nil
}

// refusePatchInsideCheckout keeps the emitted patch out of every git
// checkout: the tree changes only through a reviewed commit, and a patch
// dropped into a work tree is one `git add -A` away from skipping that.
func refusePatchInsideCheckout(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("-patch: %w", err)
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return fmt.Errorf("-patch %s is inside the checkout at %s; write it outside any checkout", path, dir)
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}
