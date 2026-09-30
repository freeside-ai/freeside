package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// authAddDeps are the side-effecting collaborators a test replaces: the Codex
// token endpoint and the container runtime.
type authAddDeps struct {
	codexRefresher ward.CodexAuthRefresher
	runtime        func(containerBin string) ward.Runtime
}

func productionAuthAddDeps() authAddDeps {
	return authAddDeps{
		codexRefresher: ward.NewCodexAuthHTTPRefresher(),
		runtime:        func(bin string) ward.Runtime { return ward.NewCLIRuntime(bin) },
	}
}

type authAddConfig struct {
	DBPath          string
	ProjectID       string
	Client          domain.HarnessClientKind
	AuthIdentityID  string
	Route           string
	CostOwner       string
	ApprovedRecipes digestSetFlag

	// Codex: the enroll-codex input and live-store flags.
	InputRoot, InputFile, AuthStoreRoot, AuthStorePath string

	// Claude: the attested account and the volume to author.
	Account, AuthVolume, ContainerBin, ExporterImage string
}

// runAuthAddCommand enrolls one harness client for a new or existing
// identity. It refuses a running daemon like enroll-codex: both paths take
// the identity's mutation lease and write a store the daemon may be reading.
func runAuthAddCommand(
	ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps authAddDeps,
) (err error) {
	cfg, err := parseAuthAddConfig(args, stderr)
	if err != nil {
		return err
	}
	var token ward.SetupToken
	if cfg.Client == domain.HarnessClientClaudeCode {
		// Read the token before opening anything, so a bad paste costs no
		// lock and no store write.
		if token, err = readSetupToken(stdin, stderr); err != nil {
			return err
		}
	}
	lock, err := requireNoDaemon("auth add", cfg.DBPath)
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
	identityID := domain.AuthIdentityID(cfg.AuthIdentityID)
	// The shape plan §5.4's lineup example uses: one enrollment per identity
	// and client. A second route on one client for one identity would collide
	// and refuse as an existing enrollment.
	enrollmentID := domain.ClientEnrollmentID(cfg.AuthIdentityID + "/" + string(cfg.Client))
	var result any
	switch cfg.Client {
	case domain.HarnessClientCodexCLI:
		result, err = ward.EnrollCodexAuth(ctx, ward.CodexAuthEnrollmentConfig{
			InputRoot: cfg.InputRoot, InputFile: cfg.InputFile,
			AuthStoreRoot: cfg.AuthStoreRoot, AuthStorePath: cfg.AuthStorePath,
			AuthIdentityID: identityID, ProjectID: domain.ProjectID(cfg.ProjectID),
			Enrollment: &ward.CodexClientEnrollmentRequest{
				EnrollmentID: enrollmentID, Route: cfg.Route, CostOwner: cfg.CostOwner,
			},
			Journal: adapters.Enrollment, AuthStoreLeaser: adapters.Leaser,
			AuthRefresher: deps.codexRefresher,
		})
	case domain.HarnessClientClaudeCode:
		result, err = ward.EnrollClaudeSetupToken(ctx, ward.ClaudeAuthEnrollmentConfig{
			Token: token, AuthIdentityID: identityID, EnrollmentID: enrollmentID,
			Route: cfg.Route, CostOwner: cfg.CostOwner, AccountBinding: cfg.Account,
			Volume: cfg.AuthVolume, Runtime: deps.runtime(cfg.ContainerBin),
			ExporterImage: cfg.ExporterImage,
			Store:         adapters.Claude, AuthStoreLeaser: adapters.Leaser,
		})
	}
	if err != nil {
		if errors.Is(err, domain.ErrUnapprovedRecipe) {
			return fmt.Errorf(
				"the store contains recipe-gated evidence; pass each approved "+
					"verification-recipe digest with -approved-recipe: %w", err)
		}
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write enrollment result: %w", err)
	}
	return nil
}

func parseAuthAddConfig(args []string, stderr io.Writer) (authAddConfig, error) {
	cfg := authAddConfig{ApprovedRecipes: digestSetFlag{}}
	var client string
	flags := flag.NewFlagSet("freesided auth add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DBPath, "db", "", "SQLite database path (required)")
	flags.StringVar(&client, "client", "", "harness client: codex_cli or claude_code (required)")
	flags.StringVar(&cfg.AuthIdentityID, "auth-identity", "", "auth identity id, new or existing (required)")
	flags.StringVar(&cfg.Route, "route", "", "route fragment id the credential is valid for (required)")
	flags.StringVar(&cfg.CostOwner, "cost-owner", "",
		"who pays for the identity's usage (required for a new or ownerless identity; must match an existing one)")
	flags.Var(&cfg.ApprovedRecipes, "approved-recipe", "approved verification-recipe digest (repeatable)")
	flags.StringVar(&cfg.ProjectID, "project", "", "codex_cli: project id for the recovery attention item")
	flags.StringVar(&cfg.InputRoot, "input-root", "", "codex_cli: private root containing the operator login")
	flags.StringVar(&cfg.InputFile, "input-file", "", "codex_cli: operator-generated auth.json under input-root")
	flags.StringVar(&cfg.AuthStoreRoot, "auth-store-root", "", "codex_cli: separate private root containing live Codex auth stores")
	flags.StringVar(&cfg.AuthStorePath, "auth-store", "", "codex_cli: live auth.json path under auth-store-root")
	flags.StringVar(&cfg.Account, "account", "",
		"claude_code: operator-attested subscription account the token belongs to")
	flags.StringVar(&cfg.AuthVolume, "auth-volume", "", "claude_code: credential volume to create")
	flags.StringVar(&cfg.ContainerBin, "container-bin", "container", "claude_code: Apple container CLI path")
	flags.StringVar(&cfg.ExporterImage, "exporter-image", "", "claude_code: digest-pinned exporter image")
	if err := flags.Parse(args); err != nil {
		return authAddConfig{}, err
	}
	if flags.NArg() != 0 {
		return authAddConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	cfg.Client = domain.HarnessClientKind(client)
	required := []struct{ name, value string }{
		{"db", cfg.DBPath}, {"client", client}, {"auth-identity", cfg.AuthIdentityID}, {"route", cfg.Route},
	}
	switch cfg.Client {
	case domain.HarnessClientCodexCLI:
		required = append(required, []struct{ name, value string }{
			{"project", cfg.ProjectID},
			{"input-root", cfg.InputRoot},
			{"input-file", cfg.InputFile},
			{"auth-store-root", cfg.AuthStoreRoot},
			{"auth-store", cfg.AuthStorePath},
		}...)
	case domain.HarnessClientClaudeCode:
		required = append(required, []struct{ name, value string }{
			{"account", cfg.Account}, {"auth-volume", cfg.AuthVolume}, {"exporter-image", cfg.ExporterImage},
		}...)
	default:
		if client != "" {
			return authAddConfig{}, fmt.Errorf("-client %q is not codex_cli or claude_code", client)
		}
	}
	for _, flag := range required {
		if flag.value == "" {
			return authAddConfig{}, fmt.Errorf("-%s is required", flag.name)
		}
	}
	return cfg, nil
}

// readSetupToken takes the Claude setup token from standard input, never
// argv, so it stays out of the process table and shell history. At a
// terminal it prompts on stderr and reads without echo; otherwise it reads
// one line, as setup -registration-code-stdin does. Errors are fixed strings:
// a reader error may carry recently read bytes.
func readSetupToken(stdin io.Reader, stderr io.Writer) (ward.SetupToken, error) {
	if stdin == nil {
		return ward.SetupToken{}, errors.New("setup token standard input is unavailable")
	}
	var payload []byte
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		_, _ = fmt.Fprint(stderr, "Setup token (from claude setup-token): ")
		line, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(stderr)
		if err != nil {
			return ward.SetupToken{}, errors.New("read setup token from the terminal")
		}
		payload = line
	} else {
		body, err := io.ReadAll(io.LimitReader(stdin, ward.MaxSetupTokenBytes+2))
		if err != nil {
			return ward.SetupToken{}, errors.New("read setup token from standard input")
		}
		payload = body
		if n := len(payload); n > 0 && payload[n-1] == '\n' {
			payload = payload[:n-1]
		}
		if n := len(payload); n > 0 && payload[n-1] == '\r' {
			payload = payload[:n-1]
		}
	}
	return ward.NewSetupToken(payload)
}
