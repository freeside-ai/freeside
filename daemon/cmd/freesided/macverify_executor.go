package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
)

// macverifyExecutorFlags registers the executor's flags. The slot and port
// base defaults match the install command's. The returned function takes the
// daemon's -listen address, whose port the executor refuses to reserve.
func macverifyExecutorFlags(flags *flag.FlagSet) func(listen string) daemoncompose.MacverifyExecutorConfig {
	root := flags.String("macverify-root", "",
		"root of the installed macOS verification executor; empty (default) configures none, so macos_host recipe commands fail")
	slots := flags.String("macverify-slots", "fvslot1,fvslot2",
		"comma-separated slot account names the executor install created")
	xcodeBuild := flags.String("macverify-xcode-build", "",
		"Xcode build number (xcodebuild -version) the executor's commands run under; required with -macverify-root")
	swift := flags.String("macverify-swift", "",
		"swiftlang- token (swift --version) the executor's commands run under; required with -macverify-root")
	portBase := flags.Int("macverify-port-base", macverify.DefaultPortBase,
		"first reserved port the executor install wrote to ports.conf (its -port-base)")
	return func(listen string) daemoncompose.MacverifyExecutorConfig {
		return daemoncompose.MacverifyExecutorConfig{
			Root: *root, Slots: engine.SplitNonEmpty(*slots), XcodeBuild: *xcodeBuild, Swift: *swift,
			PortBase: *portBase, ListenPort: listenPort(listen),
		}
	}
}

// listenPort is the port of a host:port listen address, or 0 when it names
// none or does not parse; the listener refuses an address that does not
// parse, so the executor need not.
func listenPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}

// deputyMacverify is the deputy settings file's optional macos_verification
// object: the same values as the -macverify-* flags, for the deputy, which
// takes its configuration from settings rather than flags. It feeds the
// configuration the executor's conformance pass proves and a later restore
// re-checks, so it names the executor that macos_host recipe commands run
// on. A present object must set the first four keys; the slots have no
// default, because the settings file has none. port_base alone may be
// omitted, for macverify.DefaultPortBase: no port base disables anything,
// and slot-run refuses a job whose ports.conf disagrees with it.
type deputyMacverify struct {
	Root       string   `json:"root"`
	Slots      []string `json:"slots"`
	XcodeBuild string   `json:"xcode_build"`
	Swift      string   `json:"swift"`
	PortBase   int      `json:"port_base,omitempty"`
}

// validateDeputyMacverify refuses a present object that cannot name an
// executor, one line naming the field, before the deputy takes its lease. An
// absent object (nil) passes and configures none. It refuses an empty value
// rather than reading it as "none", so a half-written object cannot quietly
// disable the executor. The toolchain tokens' formats stay with
// macverify.New, which enforces them at daemon start for the flags too.
func validateDeputyMacverify(section *deputyMacverify) error {
	if section == nil {
		return nil
	}
	if !filepath.IsAbs(section.Root) {
		return errors.New("macos_verification.root must be an absolute path")
	}
	if filepath.Clean(section.Root) != section.Root {
		return errors.New("macos_verification.root must be a clean path")
	}
	if len(section.Slots) == 0 {
		return errors.New("macos_verification.slots is empty")
	}
	for i, slot := range section.Slots {
		if slot == "" {
			return errors.New("macos_verification.slots holds an empty name")
		}
		if slices.Contains(section.Slots[:i], slot) {
			return fmt.Errorf("macos_verification.slots names %q twice", slot)
		}
	}
	if section.XcodeBuild == "" {
		return errors.New("macos_verification.xcode_build is required")
	}
	if section.Swift == "" {
		return errors.New("macos_verification.swift is required")
	}
	return nil
}

// deputyMacverifyExecutorConfig maps the settings section and the deputy's
// listen address to the executor configuration. An absent section maps to
// the zero value, whose empty Root configures no executor, as an empty
// -macverify-root does.
func deputyMacverifyExecutorConfig(section *deputyMacverify, listen string) daemoncompose.MacverifyExecutorConfig {
	if section == nil {
		return daemoncompose.MacverifyExecutorConfig{}
	}
	portBase := section.PortBase
	if portBase == 0 {
		portBase = macverify.DefaultPortBase
	}
	return daemoncompose.MacverifyExecutorConfig{
		Root: section.Root, Slots: slices.Clone(section.Slots), XcodeBuild: section.XcodeBuild, Swift: section.Swift,
		PortBase: portBase, ListenPort: listenPort(listen),
	}
}
