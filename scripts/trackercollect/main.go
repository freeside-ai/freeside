package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/freeside-ai/freeside/scripts/trackercollect/internal/collector"
)

const contractsUsage = "usage: trackercollect contracts --repo HOST/OWNER/NAME --out DIRECTORY [--cap N]"

// runContracts handles the contracts subcommand. A usage error is a hard
// failure (exit 1): exit 2 is reserved for ambiguous evidence.
func runContracts(args []string) int {
	flags := flag.NewFlagSet("contracts", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var repo string
	var out string
	var limit int
	flags.StringVar(&repo, "repo", "", "repository as HOST/OWNER/NAME")
	flags.StringVar(&out, "out", "", "output directory")
	flags.IntVar(&limit, "cap", collector.DefaultContractCap, "concurrent contract implementation cap")
	parseErr := flags.Parse(args)
	ref, err := collector.ParseRepository(repo)
	if parseErr != nil || err != nil || out == "" || limit <= 0 || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, contractsUsage)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "flags: %v\n", parseErr)
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "repository: %v\n", err)
		}
		return 1
	}
	code, err := collector.RunContracts(context.Background(), collector.ContractsConfig{
		Repository: ref,
		OutputDir:  out,
		Cap:        limit,
	}, collector.NewGHRunner(ref.Host), time.Now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trackercollect: %v\n", err)
	}
	return code
}

const unitUsage = "usage: trackercollect unit --repo HOST/OWNER/NAME --issue NUMBER --out DIRECTORY"

// runUnit handles the unit subcommand. A usage error is a hard failure
// (exit 1): exits 2 and 3 are reserved for what the report found.
func runUnit(args []string) int {
	flags := flag.NewFlagSet("unit", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var repo string
	var out string
	var issue int
	flags.StringVar(&repo, "repo", "", "repository as HOST/OWNER/NAME")
	flags.IntVar(&issue, "issue", 0, "open issue number")
	flags.StringVar(&out, "out", "", "output directory")
	parseErr := flags.Parse(args)
	ref, err := collector.ParseRepository(repo)
	if parseErr != nil || err != nil || out == "" || issue <= 0 || issue > collector.MaxGraphQLInt || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, unitUsage)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "flags: %v\n", parseErr)
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "repository: %v\n", err)
		}
		return 1
	}
	code, err := collector.RunUnit(context.Background(), collector.UnitConfig{
		Repository: ref,
		Issue:      issue,
		OutputDir:  out,
	}, collector.NewGHRunner(ref.Host), time.Now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trackercollect: %v\n", err)
	}
	return code
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "contracts" {
		os.Exit(runContracts(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "unit" {
		os.Exit(runUnit(os.Args[2:]))
	}
	var repo string
	var out string
	var pr int
	var direct bool
	flag.StringVar(&repo, "repo", "", "repository as HOST/OWNER/NAME")
	flag.IntVar(&pr, "pr", 0, "merged pull request number")
	flag.StringVar(&out, "out", "", "output directory")
	flag.BoolVar(&direct, "direct", false, "assert a prompt-backed direct unit with no closing issue")
	flag.Parse()

	ref, err := collector.ParseRepository(repo)
	if err != nil || pr <= 0 || pr > collector.MaxGraphQLInt || out == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: trackercollect --repo HOST/OWNER/NAME --pr NUMBER --out DIRECTORY [--direct]")
		if err != nil {
			fmt.Fprintf(os.Stderr, "repository: %v\n", err)
		}
		os.Exit(1)
	}

	code, err := collector.Run(context.Background(), collector.Config{
		Repository:  ref,
		PullRequest: pr,
		OutputDir:   out,
		Direct:      direct,
	}, collector.NewGHRunner(ref.Host), time.Now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trackercollect: %v\n", err)
	}
	os.Exit(code)
}
