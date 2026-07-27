// Command freeside-project-image manually exercises the reusable project-image
// builder that freesided onboard will later invoke directly.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type stringList []string

func (v *stringList) String() string { return fmt.Sprint([]string(*v)) }

func (v *stringList) Set(value string) error {
	if value == "" {
		return fmt.Errorf("value must not be empty")
	}
	*v = append(*v, value)
	return nil
}

type config struct {
	DBPath            string
	Repository        string
	RepositoryID      int64
	CommitSHA         string
	RecipePath        string
	BaseImageRef      string
	BaseBuildRef      string
	Registry          string
	LocalRegistryPort int
	ImageName         string
	RefTag            string
	GitPath           string
	ContainerPath     string
	TempDir           string
	DNS               []string
	BuildProxy        string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freeside-project-image:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := parseConfig(args, stderr)
	if err != nil {
		return err
	}
	recipe, err := os.ReadFile(cfg.RecipePath)
	if err != nil {
		return fmt.Errorf("read recipe: %w", err)
	}
	s, err := store.Open(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // primary command error wins
	builder, err := projectimage.New(projectimage.Options{
		GitPath: cfg.GitPath, ContainerPath: cfg.ContainerPath,
		TempDir: cfg.TempDir, Log: stderr,
		Record: func(recordCtx context.Context, image domain.ProjectImage) error {
			return s.WriteInternal(recordCtx, func(tx *store.InternalTx) error {
				return tx.RecordProjectImage(recordCtx, image)
			})
		},
	})
	if err != nil {
		return err
	}
	image, err := builder.Build(ctx, projectimage.Request{
		Repository: cfg.Repository, RepositoryID: cfg.RepositoryID,
		CommitSHA: cfg.CommitSHA, Recipe: recipe,
		BaseImageRef: domain.ImageRef(cfg.BaseImageRef), BaseBuildRef: cfg.BaseBuildRef,
		Registry: cfg.Registry, LocalRegistryPort: cfg.LocalRegistryPort,
		ImageName: cfg.ImageName, RefTag: cfg.RefTag, DNS: cfg.DNS,
		BuildProxy: cfg.BuildProxy,
	})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(image); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func parseConfig(args []string, output io.Writer) (config, error) {
	flags := flag.NewFlagSet("freeside-project-image", flag.ContinueOnError)
	flags.SetOutput(output)
	var cfg config
	var dns stringList
	flags.StringVar(&cfg.DBPath, "db", "", "SQLite database path for the durable build result (required)")
	flags.StringVar(&cfg.Repository, "repository", "", "canonical owner/name repository (required)")
	flags.Int64Var(&cfg.RepositoryID, "repository-id", 0, "canonical numeric GitHub repository ID (required)")
	flags.StringVar(&cfg.CommitSHA, "commit", "", "exact full lowercase commit SHA (required)")
	flags.StringVar(&cfg.RecipePath, "recipe", "", "trusted verification recipe JSON file (required)")
	flags.StringVar(&cfg.BaseImageRef, "base-image", "", "digest-pinned approved agent base (required)")
	flags.StringVar(&cfg.BaseBuildRef, "base-build-ref", "", "local base tag whose digest must match -base-image (required)")
	flags.StringVar(&cfg.Registry, "registry", "", "registry host/path destination")
	flags.IntVar(&cfg.LocalRegistryPort, "local-registry-port", 0,
		"managed loopback registry port retained to back the returned reference (1024-65535)")
	flags.StringVar(&cfg.ImageName, "image-name", "", "project image name (default derived from repository)")
	flags.StringVar(&cfg.RefTag, "ref-tag", "v1", "prefix for the one-shot image push tag")
	flags.StringVar(&cfg.GitPath, "git", "", "git executable (default from PATH)")
	flags.StringVar(&cfg.ContainerPath, "container", "", "Apple container executable (default from PATH)")
	flags.StringVar(&cfg.TempDir, "temp-dir", "", "bindable scratch parent (default OS temporary directory)")
	flags.Var(&dns, "dns", "build DNS server; repeatable")
	flags.StringVar(&cfg.BuildProxy, "build-proxy", "",
		"optional build-only HTTP proxy URL without credentials")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	cfg.DNS = append([]string{}, dns...)
	required := []struct {
		name  string
		value string
	}{
		{"-db", cfg.DBPath},
		{"-repository", cfg.Repository},
		{"-commit", cfg.CommitSHA},
		{"-recipe", cfg.RecipePath},
		{"-base-image", cfg.BaseImageRef},
		{"-base-build-ref", cfg.BaseBuildRef},
	}
	for _, field := range required {
		if field.value == "" {
			return config{}, fmt.Errorf("%s is required", field.name)
		}
	}
	if cfg.RepositoryID <= 0 {
		return config{}, fmt.Errorf("-repository-id must be positive")
	}
	return cfg, nil
}
