package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/macverify"
)

var (
	errMacverifyUsage     = errors.New("invalid macverify command")
	errMacverifyUnhealthy = errors.New("the executor install is incomplete")
)

// runMacverifyMain installs, checks, or removes the macOS verification
// executor's host state. Only flag parsing and output live here; every
// create, adopt, and delete decision is internal/macverify's.
//
// The hidden probe verb is the confinement probe the executor's room runs as
// a slot, from a copy of this binary, so the probe is the build the
// conformance record names. It needs no host setup and prints only its
// report.
func runMacverifyMain(args []string) {
	if len(args) > 0 && args[0] == "probe" {
		os.Exit(macverify.RunProbe(args[1:], os.Stdout))
	}
	host, err := macverify.NewSystemHost()
	if err == nil {
		err = runMacverifyCommand(args, os.Stdout, host, defaultMacverifyOperator())
	}
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "freesided macverify:", err)
	if errors.Is(err, errMacverifyUsage) {
		os.Exit(2)
	}
	os.Exit(1)
}

// defaultMacverifyOperator is the user the daemon runs as: the one who ran
// sudo, or the current user when check runs without it.
func defaultMacverifyOperator() string {
	if name := os.Getenv("SUDO_USER"); name != "" {
		return name
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func parseMacverifySpec(verb string, args []string, operator string) (macverify.InstallSpec, error) {
	spec := macverify.InstallSpec{}
	flags := flag.NewFlagSet("freesided macverify "+verb, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&spec.Root, "root", "/Library/Freeside/macverify", "executor root")
	flags.IntVar(&spec.SlotCount, "slots", 2, "number of slot accounts")
	flags.StringVar(&spec.SlotPrefix, "slot-prefix", "fvslot", "slot account name prefix; slots are <prefix>1 to <prefix><slots>")
	flags.StringVar(&spec.Group, "group", "fvslots", "slot group name")
	flags.IntVar(&spec.FirstID, "first-id", 460, "the slot group's gid; slot n gets uid first-id+n")
	flags.StringVar(&spec.Operator, "operator", operator, "the user the daemon runs as (default: $SUDO_USER)")
	flags.StringVar(&spec.Anchor, "anchor", "freeside-macverify", "pf anchor name under com.apple/")
	flags.StringVar(&spec.Label, "label", "ai.freeside.macverify.pf", "boot LaunchDaemon label")
	flags.StringVar(&spec.SudoersName, "sudoers-name", "freeside-slot-run", "sudoers drop-in name under /etc/sudoers.d")
	flags.IntVar(&spec.PortBase, "port-base", macverify.DefaultPortBase,
		"first reserved port; each slot reserves four, below the ephemeral range")
	if err := flags.Parse(args); err != nil {
		return spec, fmt.Errorf("%w: %w", errMacverifyUsage, err)
	}
	if flags.NArg() != 0 {
		return spec, fmt.Errorf("%w: unexpected argument %q", errMacverifyUsage, flags.Arg(0))
	}
	return spec, nil
}

func runMacverifyCommand(args []string, stdout io.Writer, host macverify.Host, operator string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: expected install, check, or remove", errMacverifyUsage)
	}
	verb := args[0]
	spec, err := parseMacverifySpec(verb, args[1:], operator)
	if err != nil {
		return err
	}
	switch verb {
	case "install":
		findings, err := macverify.Install(spec, host)
		if err != nil {
			return errors.Join(err, writeLines(stdout, findingLines(findings)))
		}
		cfg := spec.Config()
		return writeLines(stdout, []string{
			"installed " + spec.Root,
			fmt.Sprintf("macverify.Config: ExecutorRoot %s, Slots %s, PortBase %d",
				cfg.ExecutorRoot, strings.Join(cfg.Slots, ","), cfg.PortBase),
		})
	case "check":
		findings, err := macverify.Inspect(spec, host)
		if err := writeLines(stdout, findingLines(findings)); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		for _, f := range findings {
			if !f.Healthy() {
				return errMacverifyUnhealthy
			}
		}
		return nil
	case "remove":
		report, err := macverify.Remove(spec, host)
		var lines []string
		for _, removed := range report.Removed {
			lines = append(lines, "removed "+removed)
		}
		for _, left := range report.Left {
			lines = append(lines, "left "+left)
		}
		return errors.Join(err, writeLines(stdout, lines))
	}
	return fmt.Errorf("%w: unknown verb %q; expected install, check, or remove", errMacverifyUsage, verb)
}

func findingLines(findings []macverify.Finding) []string {
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		line := fmt.Sprintf("%-13s %s", f.State, f.Piece)
		if f.Detail != "" {
			line += ": " + f.Detail
		}
		lines = append(lines, line)
	}
	return lines
}

func writeLines(w io.Writer, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return err
}
