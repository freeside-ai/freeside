package main

import (
	"flag"
	"net"
	"slices"
	"strconv"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
// object, the instance settings' section of the same name, so the file and
// the store hold one definition. A present object must set every key but
// port_base; see domain.MacosVerificationSettings.
type deputyMacverify = domain.MacosVerificationSettings

// validateDeputyMacverify refuses a present object that cannot name an
// executor, one line naming the field, before the deputy takes its lease. An
// absent object (nil) passes and configures none.
func validateDeputyMacverify(section *deputyMacverify) error {
	if section == nil {
		return nil
	}
	return section.Validate()
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
