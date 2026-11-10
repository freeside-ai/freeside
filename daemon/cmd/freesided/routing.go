package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// routingPolicyRoute carries both verbs: the args it forwards name the verb,
// so the running daemon runs the same dispatch.
const routingPolicyRoute = "/routing-policy"

// runRoutingMain is the routing policy verb (plan §5.4, routing policy):
// `routing set` records a new revision and `routing show` prints the current
// one. The router reads the current revision at a task's first admission.
func runRoutingMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runRoutingCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided routing:", err)
		os.Exit(1)
	}
}

func runRoutingCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: freesided routing set|show [flags]")
	}
	switch args[0] {
	case "set":
		return runRoutingSetCommand(ctx, args, stdout, stderr)
	case "show":
		return runRoutingShowCommand(ctx, args, stdout, stderr)
	}
	return errors.New("unknown routing command (want set or show)")
}

// routingOrderFlag collects repeated -order role=enrollment1,enrollment2
// values. It checks only that form; the engine validates the set. The ids
// are comma-separated, so an id holding a comma cannot be ordered here, and
// an empty one is refused rather than guessed at.
type routingOrderFlag []domain.RoutingPolicyRole

func (f *routingOrderFlag) String() string { return "" }

func (f *routingOrderFlag) Set(value string) error {
	role, list, found := strings.Cut(value, "=")
	if !found || role == "" || list == "" {
		return errors.New("want role=enrollment1,enrollment2")
	}
	var enrollments []domain.ClientEnrollmentID
	for id := range strings.SplitSeq(list, ",") {
		if id == "" {
			return errors.New("want role=enrollment1,enrollment2 with no empty enrollment id")
		}
		enrollments = append(enrollments, domain.ClientEnrollmentID(id))
	}
	*f = append(*f, domain.RoutingPolicyRole{Role: domain.RoleName(role), Enrollments: enrollments})
	return nil
}

type routingSetConfig struct {
	DBPath            string
	AgentTreeCheckout string
	AgentTreeCommit   string
	Orders            routingOrderFlag
	// DrainFirst is the drain-first account's usage pool; empty names none.
	DrainFirst string
}

// routingSetResult reports the request. Recorded is false when the current
// revision already held the orders and account, so nothing was written. Policy is the
// current revision after the request.
type routingSetResult struct {
	Recorded bool                 `json:"recorded"`
	Policy   domain.RoutingPolicy `json:"policy"`
}

// runRoutingSetCommand records a new revision validated against the store's
// enrollments and the lineup of the committed tree at -agent-tree-commit,
// which gives each ordered role the model and effort its enrollments run. A running daemon runs it from its forwarded
// args, so the daemon's process reads the tree and the record names its
// account, which the control socket's peer check makes the caller's.
func runRoutingSetCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	cfg, err := parseRoutingSetConfig(args[1:], stderr)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, routingPolicyRoute, args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	tree, revision, err := daemoncompose.ReadAgentTree(ctx, cfg.AgentTreeCheckout, cfg.AgentTreeCommit)
	if err != nil {
		return err
	}
	account, err := user.Current()
	if err != nil {
		return fmt.Errorf("current account: %w", err)
	}
	// Every set is the whole policy, so a set without -drain-first clears
	// any account the current revision names.
	var drainFirst *domain.UsagePoolID
	if cfg.DrainFirst != "" {
		drainFirst = new(domain.UsagePoolID(cfg.DrainFirst))
	}
	set, err := engine.SetRoutingPolicy(ctx, handle.store, tree, revision, engine.SetRoutingPolicyRequest{
		Roles: cfg.Orders, DrainFirstAccount: drainFirst,
		Source: domain.RoutingPolicySourceCLISet, SetBy: "cli:" + account.Username,
	}, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(routingSetResult{Recorded: set.Recorded, Policy: set.Policy}); err != nil {
		return fmt.Errorf("write routing policy result: %w", err)
	}
	return nil
}

func parseRoutingSetConfig(args []string, output io.Writer) (routingSetConfig, error) {
	var cfg routingSetConfig
	flags := flag.NewFlagSet("freesided routing set", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.StringVar(&cfg.AgentTreeCheckout, "agent-tree", "",
		"absolute path of a local checkout holding the admitted-agent tree under policy/ (required)")
	flags.StringVar(&cfg.AgentTreeCommit, "agent-tree-commit", "", "exact commit of -agent-tree whose tree is read (required)")
	flags.Var(&cfg.Orders, "order",
		"role=enrollment1,enrollment2: the role's enrollment ids in fallback order, each run on the role's "+
			"lineup model and effort; repeat per role (required)")
	flags.StringVar(&cfg.DrainFirst, "drain-first", "",
		"usage pool that takes every new task ahead of reset order; an ordered enrollment's identity must be on it "+
			"(optional; omitting it clears the current account)")
	if err := flags.Parse(args); err != nil {
		return routingSetConfig{}, err
	}
	switch {
	case flags.NArg() != 0:
		return routingSetConfig{}, errors.New("unexpected positional arguments")
	case cfg.DBPath == "":
		return routingSetConfig{}, errors.New("-db is required")
	case cfg.AgentTreeCheckout == "":
		return routingSetConfig{}, errors.New("-agent-tree is required")
	// A running daemon reads this path from its own working directory, so a
	// relative one would name a different checkout there than here.
	case !filepath.IsAbs(cfg.AgentTreeCheckout):
		return routingSetConfig{}, errors.New("-agent-tree must be an absolute path")
	case cfg.AgentTreeCommit == "":
		return routingSetConfig{}, errors.New("-agent-tree-commit is required")
	case len(cfg.Orders) == 0:
		return routingSetConfig{}, errors.New("-order is required")
	}
	return cfg, nil
}

// runRoutingShowCommand prints the current revision, null when none was set,
// or with -revisions every revision in order.
func runRoutingShowCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided routing show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "existing SQLite database path (required)")
	revisions := flags.Bool("revisions", false, "print every revision instead of the current one")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *dbPath == "" {
		return errors.New("-db is required")
	}
	handle, client, err := openCommandStore(ctx, *dbPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, routingPolicyRoute, args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	var output any
	if err := handle.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if *revisions {
			output, err = tx.RoutingPolicyRevisions(ctx)
			return err
		}
		output, err = currentRoutingPolicyOrNil(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		return fmt.Errorf("write routing policy: %w", err)
	}
	return nil
}

func currentRoutingPolicyOrNil(ctx context.Context, tx *store.ReadTx) (*domain.RoutingPolicy, error) {
	policy, err := tx.CurrentRoutingPolicy(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &policy, nil
}
