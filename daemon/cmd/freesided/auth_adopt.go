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
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
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

	ClaudeRoute, CodexRoute, ReviewModel string
	// ReviewHarness is the harness the reviewer line's agent runs (#397):
	// codex_cli adopts the review identity's Codex store, and claude_code
	// its setup-token volume.
	ReviewHarness                                  domain.HarnessClientKind
	TermsBasisDate, PricingRevision, OfferNotAfter string
	// ReviewEffort, ShadowReviewModel, and ShadowReviewEffort are what the
	// review agents name beside ReviewModel. An effort is a Freeside level.
	ReviewEffort, ShadowReviewEffort domain.EffortLevel
	ShadowReviewModel                string
	// The daemon's three prompt-package files; the lineup lines record their
	// digests. The review prompt is code-owned and needs no file.
	PromptPackage, SpecificationPromptPackage, RemediationPromptPackage string
	// Writers is what each writer role's agent launches with: the exact
	// Claude model id its offer names and the effort it asks. Every writer
	// role is required, because a writer launch passes both and the tree
	// refuses a line that would leave either to the CLI.
	Writers map[domain.RoleName]agentbaseline.Selection
	// JudgmentModel is the exact Claude model id the call offer names, and
	// JudgmentEffort the effort every judgment call asks. The daemon has no
	// flag for either: a judgment call launches with what its lineup line's
	// agent and offer name. An empty model means no judgment lines.
	JudgmentModel  string
	JudgmentEffort domain.EffortLevel
	// JudgmentPublicationAuthorPrompt is the daemon's flag of the same name.
	// The other judgment roles' prompts are code-owned and need no file.
	JudgmentPublicationAuthorPrompt string
	PatchPath                       string
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
	// Enabled reports an identity that was stored disabled and that this run
	// enabled in its first-enrollment write.
	Enabled bool `json:"enabled,omitempty"`
	// Disabled reports a reused identity that is stored disabled. Adoption
	// enables only at first enrollment, so a disable recorded after it stands,
	// and agent resolution refuses the identity until it is enabled.
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

	var codexEnrollment, claudeReviewEnrollment *domain.ClientEnrollment
	// adoptReview reports the review identity's adoption beside the writer's
	// and returns its enrollment, nil when the identity is unadoptable.
	adoptReview := func(result ward.AuthAdoptionResult, adoptErr error) (*domain.ClientEnrollment, error) {
		entry, err := adoptReportEntry(reviewIdentity, cfg.ReviewHarness, result, adoptErr)
		if err != nil {
			// The first identity's adoption is already recorded; say so
			// before failing, so the operator knows a rerun reuses it.
			return nil, errors.Join(err, writeAdoptReport(reportOut, report))
		}
		report.Identities = append(report.Identities, entry)
		if err := retireUnadoptable(ctx, st, cfg, &report.Identities[1], deps.now()); err != nil {
			return nil, errors.Join(err, writeAdoptReport(reportOut, report))
		}
		if adoptErr != nil {
			return nil, nil
		}
		return &result.Enrollment, nil
	}
	switch {
	case reviewID == claudeID && cfg.ReviewHarness == domain.HarnessClientClaudeCode:
		// One identity under both flags is one adoption. A Claude reviewer
		// then runs on the writer's enrollment, under its own agent.
		if claudeErr == nil {
			claudeReviewEnrollment = &claude.Enrollment
		}
	case reviewID == claudeID:
		// The one store is the Claude one, so a Codex reviewer has no store
		// and the tree carries no review agent.
	case cfg.ReviewHarness == domain.HarnessClientClaudeCode:
		// The review identity is a Claude setup-token identity auth add
		// enrolled; adoption reuses that enrollment. An identity with none
		// is unadoptable here, because no account is attested for it.
		review, reviewErr := ward.AdoptClaudeAuth(ctx, ward.ClaudeAuthAdoptionConfig{
			AuthAdoptionConfig: shared(reviewIdentity, domain.HarnessClientClaudeCode, cfg.ClaudeRoute, cfg.ReviewCostOwner),
			ObserveVolume:      deps.observeVolume(cfg.ContainerBin, cfg.ExporterImage),
		})
		if claudeReviewEnrollment, err = adoptReview(review, reviewErr); err != nil {
			return err
		}
	default:
		codex, codexErr := ward.AdoptCodexAuth(ctx, ward.CodexAuthAdoptionConfig{
			AuthAdoptionConfig: shared(reviewIdentity, domain.HarnessClientCodexCLI, cfg.CodexRoute, cfg.ReviewCostOwner),
			AuthStoreRoot:      cfg.AuthStoreRoot,
		})
		if codexEnrollment, err = adoptReview(codex, codexErr); err != nil {
			return err
		}
	}
	if codexEnrollment != nil || claudeReviewEnrollment != nil {
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
		ClaudeReviewEnrollment: claudeReviewEnrollment,
		ReviewModel:            cfg.ReviewModel, ReviewEffort: cfg.ReviewEffort,
		ShadowReviewModel: cfg.ShadowReviewModel, ShadowReviewEffort: cfg.ShadowReviewEffort,
		Writers: cfg.Writers, JudgmentModel: cfg.JudgmentModel, JudgmentEffort: cfg.JudgmentEffort,
		TermsBasisDate: cfg.TermsBasisDate, PricingRevision: cfg.PricingRevision,
		OfferNotAfter: notAfter, Prompts: prompts,
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
	// identity is the value read before adoption. The command holds the daemon
	// lock, so nothing else wrote the bit: a first enrollment enabled it, and a
	// reused enrollment left it as read.
	entry.Status = authAdoptAdopted
	if result.Reused {
		entry.Status = authAdoptReused
	}
	entry.EnrollmentID, entry.Generation = result.Enrollment.ID, result.Generation.Ordinal
	entry.Enabled = !result.Reused && !identity.Enabled
	entry.Disabled = result.Reused && !identity.Enabled
	return entry, nil
}

// readAdoptPrompts digests the daemon's prompt packages into the writer
// roles' lineup prompts: the role name, with the digest admission records for
// the package (the content address of its bytes). With a judgment model,
// each judgment role gets its code-owned prompt; the publication author gets
// a prompt, and so a line, only when its prompt file is named, because
// without the file the daemon leaves that role off. Without a judgment model
// the tree has no call agent, so no judgment role gets a prompt or a line.
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
	if cfg.JudgmentModel == "" {
		return prompts, nil
	}
	for _, role := range agentbaseline.JudgmentRoles() {
		prompt, ok := inference.CodeOwnedRolePrompt(role)
		if !ok {
			continue
		}
		prompts[role] = agentbaseline.Prompt{Name: prompt.Name, Digest: prompt.Digest}
	}
	if cfg.JudgmentPublicationAuthorPrompt != "" {
		body, err := readJudgmentPrompt(cfg.JudgmentPublicationAuthorPrompt)
		if err != nil {
			return nil, fmt.Errorf("-judgment-publication-author-prompt: %w", err)
		}
		prompt := inference.OperatorRolePrompt(domain.RolePublicationAuthor, body)
		prompts[domain.RolePublicationAuthor] = agentbaseline.Prompt{Name: prompt.Name, Digest: prompt.Digest}
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
	cfg := authAdoptConfig{
		ApprovedRecipes: digestSetFlag{},
		Writers:         make(map[domain.RoleName]agentbaseline.Selection, len(agentbaseline.WriterRoles)),
	}
	flags := flag.NewFlagSet("freesided auth adopt", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DBPath, "db", "", "SQLite database path (required)")
	flags.Var(&cfg.ApprovedRecipes, "approved-recipe", "approved verification-recipe digest (repeatable)")
	flags.StringVar(&cfg.AuthIdentityID, "auth-identity", "",
		"the flag-era -auth-identity: the Claude identity tasks ran under (required)")
	flags.StringVar(&cfg.ReviewAuthIdentityID, "review-auth-identity", "",
		"the flag-era -review-auth-identity: the review identity (required)")
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
	for _, role := range agentbaseline.WriterRoles {
		flags.Func(string(role)+"-model",
			fmt.Sprintf("exact Claude model id the %s's offer names (required)", role),
			func(model string) error {
				selection := cfg.Writers[role]
				selection.Model = model
				cfg.Writers[role] = selection
				return nil
			})
		flags.Func(string(role)+"-effort",
			fmt.Sprintf("effort the %s asks, one of %v (required)", role, explicitEfforts),
			func(effort string) error {
				selection := cfg.Writers[role]
				selection.Effort = domain.EffortLevel(effort)
				cfg.Writers[role] = selection
				return nil
			})
	}
	flags.StringVar((*string)(&cfg.ReviewHarness), "review-harness", string(domain.HarnessClientCodexCLI),
		"harness the reviewer runs: codex_cli adopts the review identity's Codex store, "+
			"claude_code its setup-token volume")
	flags.StringVar(&cfg.ReviewModel, "review-model", "",
		"model the review agent's offer names: a Codex model, or the exact Claude model id under -review-harness claude_code")
	flags.StringVar((*string)(&cfg.ReviewEffort), "review-effort", "",
		fmt.Sprintf("effort the review agent asks, one of %v", explicitEfforts))
	flags.StringVar(&cfg.ShadowReviewModel, "shadow-review-model", "",
		"exact Claude model id the shadow review offer names; set exactly while the shadow arm is on")
	flags.StringVar((*string)(&cfg.ShadowReviewEffort), "shadow-review-effort", "",
		fmt.Sprintf("effort the shadow review agent asks, one of %v; set exactly while the shadow arm is on", explicitEfforts))
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
	flags.StringVar(&cfg.JudgmentModel, "judgment-model", "",
		"exact Claude model id the call offer names, which every judgment call launches with "+
			"(optional; without it the patch has no judgment lines)")
	flags.StringVar((*string)(&cfg.JudgmentEffort), "judgment-effort", "",
		fmt.Sprintf("effort every judgment call asks, one of %v; set exactly with -judgment-model", explicitEfforts))
	flags.StringVar(&cfg.JudgmentPublicationAuthorPrompt, "judgment-publication-author-prompt", "",
		"the daemon's -judgment-publication-author-prompt (optional, and needs -judgment-model; "+
			"without it the publication author gets no line)")
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
	if err := validateReviewHarness(cfg, oneIdentity); err != nil {
		return authAdoptConfig{}, err
	}
	if err := validateWriterSelections(cfg.Writers); err != nil {
		return authAdoptConfig{}, err
	}
	if cfg.ReviewEffort != "" {
		if err := validateExplicitEffort("review-effort", cfg.ReviewEffort); err != nil {
			return authAdoptConfig{}, err
		}
	}
	if cfg.ReviewModel != "" && !validReviewLaunchModel(cfg.ReviewModel) {
		return authAdoptConfig{}, errors.New("-review-model cannot be passed to the review launch as one argument")
	}
	if err := validateShadowReviewSelection(cfg); err != nil {
		return authAdoptConfig{}, err
	}
	// The call launch passes the model as one option value and always passes
	// one, so a value it could not pass, or the id that names no model, would
	// write judgment lines that never admit.
	if !inference.ValidLaunchValue(cfg.JudgmentModel) {
		return authAdoptConfig{}, errors.New(
			"-judgment-model cannot be passed to a launch as one option value")
	}
	if cfg.JudgmentModel == domain.ClaudeCodeNativeDefaultRouteModelID {
		return authAdoptConfig{}, fmt.Errorf(
			"-judgment-model %s names no model: give the exact Claude model id", cfg.JudgmentModel)
	}
	// A call launch passes the effort too, so the call agent asks one; the
	// effort is meaningless without the model it launches beside.
	if (cfg.JudgmentModel == "") != (cfg.JudgmentEffort == "") {
		return authAdoptConfig{}, errors.New(
			"-judgment-model and -judgment-effort are set together: a judgment call launches with both")
	}
	if cfg.JudgmentEffort != "" {
		if err := validateExplicitEffort("judgment-effort", cfg.JudgmentEffort); err != nil {
			return authAdoptConfig{}, err
		}
	}
	if cfg.JudgmentPublicationAuthorPrompt != "" && cfg.JudgmentModel == "" {
		return authAdoptConfig{}, errors.New(
			"-judgment-publication-author-prompt needs -judgment-model: without a model the patch has no judgment lines")
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

// validateReviewHarness holds the review flags to the harness the reviewer
// runs. A Codex reviewer on its own identity needs the Codex store root; a
// Claude reviewer adopts a setup-token volume, so a store root names nothing
// it uses, and it always gets a review agent, so it always needs the model
// and effort that agent's offer names.
func validateReviewHarness(cfg authAdoptConfig, oneIdentity bool) error {
	switch cfg.ReviewHarness {
	case domain.HarnessClientCodexCLI:
		if !oneIdentity && (cfg.AuthStoreRoot == "" || cfg.ReviewModel == "" || cfg.ReviewEffort == "") {
			return errors.New(
				"-auth-store-root, -review-model, and -review-effort are required to adopt the review identity")
		}
		return nil
	case domain.HarnessClientClaudeCode:
		if cfg.AuthStoreRoot != "" {
			return errors.New(
				"-auth-store-root names a Codex store: a claude_code reviewer adopts its identity's setup-token volume")
		}
		if cfg.ReviewModel == "" || cfg.ReviewEffort == "" {
			return errors.New("-review-model and -review-effort are required for a claude_code reviewer")
		}
		if cfg.ReviewModel == domain.ClaudeCodeNativeDefaultRouteModelID {
			return fmt.Errorf("-review-model %s names no model: give the exact Claude model id", cfg.ReviewModel)
		}
		return nil
	}
	return fmt.Errorf("-review-harness %q is not codex_cli or claude_code", cfg.ReviewHarness)
}

// explicitEfforts are the efforts a baseline agent can ask: every Freeside
// level but harness_default, which sends no effort, and every baseline
// launch passes one.
var explicitEfforts = slices.DeleteFunc(slices.Clone(domain.AllEffortLevels), func(effort domain.EffortLevel) bool {
	return effort == domain.EffortHarnessDefault
})

func validateExplicitEffort(flagName string, effort domain.EffortLevel) error {
	if !slices.Contains(explicitEfforts, effort) {
		return fmt.Errorf("-%s %q is not an effort a launch can pass: give one of %v",
			flagName, effort, explicitEfforts)
	}
	return nil
}

// validateWriterSelections holds every writer role to a model and an effort
// the writer launch can pass. A writer launch passes both, so a role with
// either missing would write a line that runs on the CLI's own default,
// which is the selection the baseline exists to replace.
func validateWriterSelections(writers map[domain.RoleName]agentbaseline.Selection) error {
	for _, role := range agentbaseline.WriterRoles {
		selection := writers[role]
		if selection.Model == "" {
			return fmt.Errorf("-%s-model is required", role)
		}
		if selection.Effort == "" {
			return fmt.Errorf("-%s-effort is required", role)
		}
		if selection.Model == domain.ClaudeCodeNativeDefaultRouteModelID {
			return fmt.Errorf("-%s-model %s names no model: give the exact Claude model id", role, selection.Model)
		}
		if !validClaudeLaunchModel(selection.Model) {
			return fmt.Errorf("-%s-model cannot be passed to the writer launch as one option value", role)
		}
		if err := validateExplicitEffort(string(role)+"-effort", selection.Effort); err != nil {
			return err
		}
	}
	return nil
}

// validClaudeLaunchModel reports whether a Claude writer launch can pass the
// model as one --model value: the shape every launch value has, and the
// launch's own rule against a CLI delimiter (exec/claude refuses a comma
// rather than escaping it). Adoption holds its flags to it so a tree it
// wrote is never refused at launch for its model's shape.
func validClaudeLaunchModel(model string) bool {
	return inference.ValidLaunchValue(model) && !strings.Contains(model, ",")
}

// validateShadowReviewSelection holds the shadow reviewer's model and effort
// to the shadow arm: both are needed exactly while -shadow-review-cost-owner
// turns the arm on, because the tree has a shadow reviewer agent only then.
func validateShadowReviewSelection(cfg authAdoptConfig) error {
	if cfg.ShadowReviewCostOwner == "" {
		if cfg.ShadowReviewModel != "" || cfg.ShadowReviewEffort != "" {
			return errors.New(
				"-shadow-review-model and -shadow-review-effort need -shadow-review-cost-owner: without it the patch has no shadow reviewer")
		}
		return nil
	}
	if cfg.ShadowReviewModel == "" || cfg.ShadowReviewEffort == "" {
		return errors.New(
			"-shadow-review-model and -shadow-review-effort are required with -shadow-review-cost-owner")
	}
	if err := validateExplicitEffort("shadow-review-effort", cfg.ShadowReviewEffort); err != nil {
		return err
	}
	if cfg.ShadowReviewModel == domain.ClaudeCodeNativeDefaultRouteModelID {
		return fmt.Errorf("-shadow-review-model %s names no model: give the exact Claude model id", cfg.ShadowReviewModel)
	}
	if !validReviewLaunchModel(cfg.ShadowReviewModel) {
		return errors.New("-shadow-review-model cannot be passed to the review launch as one argument")
	}
	return nil
}

// validReviewLaunchModel reports whether a review launch can pass the model
// as one command-line argument: the shape every launch value has, and the
// review launch's own rule. Adoption holds its flags to it and the daemon
// holds a review role's offer to it (reviewRoleAgent), so a tree adoption
// wrote is never refused for its model's shape.
func validReviewLaunchModel(model string) bool {
	return inference.ValidLaunchValue(model) &&
		ward.ValidateCodexReviewModelConfiguration(model, string(domain.EffortHigh)) == nil
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
