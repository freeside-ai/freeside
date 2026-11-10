package main

import (
	"flag"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
)

// macverifyExecutorFlags registers the executor's flags. The slot default
// matches the install command's default slots.
func macverifyExecutorFlags(flags *flag.FlagSet) func() daemoncompose.MacverifyExecutorConfig {
	root := flags.String("macverify-root", "",
		"root of the installed macOS verification executor; empty (default) configures none, so macos_host recipe commands fail")
	slots := flags.String("macverify-slots", "fvslot1,fvslot2",
		"comma-separated slot account names the executor install created")
	xcodeBuild := flags.String("macverify-xcode-build", "",
		"Xcode build number (xcodebuild -version) the executor's commands run under; required with -macverify-root")
	swift := flags.String("macverify-swift", "",
		"swiftlang- token (swift --version) the executor's commands run under; required with -macverify-root")
	return func() daemoncompose.MacverifyExecutorConfig {
		return daemoncompose.MacverifyExecutorConfig{
			Root: *root, Slots: engine.SplitNonEmpty(*slots), XcodeBuild: *xcodeBuild, Swift: *swift,
		}
	}
}
