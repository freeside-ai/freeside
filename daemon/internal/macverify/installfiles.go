package macverify

import (
	"bytes"
	"fmt"
	"strings"
)

// sudoersHeader opens the drop-in install writes; removal matches on it.
const sudoersHeader = "# freeside macverify: lets the daemon's user run the slot-run helper as root.\n" +
	"# Written by freesided macverify install; removed by freesided macverify remove.\n"

const sudoersRuleInfix = " ALL=(root) NOPASSWD: "

// renderSudoers is the drop-in granting operator the one helper path, the
// sole root door the room uses (devlog/2026-11-09-0700-macos-slot-launch-path.md).
func renderSudoers(operator, root string) []byte {
	return []byte(sudoersHeader + operator + sudoersRuleInfix + helperPath(root) + "\n")
}

// sudoersOperator reports the operator a drop-in grants when its content is
// exactly what install writes for root, for any valid operator name. The
// bare rule without the header is the drop-in the hand setup in
// daemon/README.md wrote before install existed, so install adopts it too.
func sudoersOperator(content []byte, root string) (string, bool) {
	rule := strings.TrimPrefix(string(content), sudoersHeader)
	operator, _, ok := strings.Cut(rule, sudoersRuleInfix)
	if !ok || !operatorPattern.MatchString(operator) {
		return "", false
	}
	want := renderSudoers(operator, root)
	return operator, bytes.Equal(content, want) || rule == strings.TrimPrefix(string(want), sudoersHeader)
}

// renderSlotsConf is the helper's slot list: one account name per line.
func renderSlotsConf(slots []string) []byte {
	return []byte(strings.Join(slots, "\n") + "\n")
}

// renderBootPlist is the LaunchDaemon that reloads the slot anchor at boot.
// Stock macOS loads /etc/pf.conf at boot (com.apple.pfctl runs pfctl -f) but
// never enables pf, so the job both loads the anchor under com.apple and
// takes an enable reference. Every interpolated value is validated against a
// pattern with no XML metacharacter (InstallSpec.validate).
func renderBootPlist(label, anchor, anchorFile string) []byte {
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>/sbin/pfctl</string>
		<string>-E</string>
		<string>-a</string>
		<string>%s</string>
		<string>-f</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`, label, anchor, anchorFile)
}

// bootPlistShaped reports whether a plist at install's path is one install
// wrote, perhaps for other names or by an older daemon: it carries install's
// label and runs pfctl. Install overwrites such a plist; anything else at
// that path is left alone.
func bootPlistShaped(content []byte, label string) bool {
	s := string(content)
	return strings.Contains(s, "<key>Label</key>\n\t<string>"+label+"</string>\n") &&
		strings.Contains(s, "<string>/sbin/pfctl</string>")
}

// helperShaped reports whether content is some version of slot-run. Removal
// deletes an executor root only when it holds one, so a mistyped -root never
// names a tree to delete.
func helperShaped(content []byte) bool {
	return bytes.HasPrefix(content, []byte("#!/bin/sh\n# slot-run: "))
}
