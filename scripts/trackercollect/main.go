package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/freeside-ai/freeside/scripts/trackercollect/internal/collector"
)

func main() {
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
