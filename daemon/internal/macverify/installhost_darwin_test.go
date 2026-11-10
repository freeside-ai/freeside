//go:build darwin

package macverify

import (
	"strings"
	"testing"
)

// dsclSlotRecord is `dscl -plist . -read /Users/fvslot1 UniqueID
// PrimaryGroupID NFSHomeDirectory UserShell Password IsHidden`, captured on
// the reference host (macOS 27.2). dscl names IsHidden with the native
// prefix and the rest with the standard one (#3009).
const dsclSlotRecord = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>dsAttrTypeNative:IsHidden</key>
	<array>
		<string>1</string>
	</array>
	<key>dsAttrTypeStandard:NFSHomeDirectory</key>
	<array>
		<string>/Library/Freeside/macverify/homes/fvslot1</string>
	</array>
	<key>dsAttrTypeStandard:Password</key>
	<array>
		<string>*</string>
	</array>
	<key>dsAttrTypeStandard:PrimaryGroupID</key>
	<array>
		<string>460</string>
	</array>
	<key>dsAttrTypeStandard:UniqueID</key>
	<array>
		<string>461</string>
	</array>
	<key>dsAttrTypeStandard:UserShell</key>
	<array>
		<string>/usr/bin/false</string>
	</array>
</dict>
</plist>
`

func TestParseDSPlistReadsNativeAndStandardAttributes(t *testing.T) {
	t.Parallel()
	record, err := parseDSPlist([]byte(dsclSlotRecord))
	if err != nil {
		t.Fatal(err)
	}
	a, err := accountFromRecord("fvslot1", record)
	if err != nil {
		t.Fatal(err)
	}
	want := Account{
		Name: "fvslot1", UID: 461, PrimaryGID: 460, Home: "/Library/Freeside/macverify/homes/fvslot1",
		Shell: "/usr/bin/false", Password: "*", Hidden: true,
	}
	if a != want {
		t.Fatalf("account = %+v, want %+v", a, want)
	}
}

// launchctlPrintFinished is `launchctl print system/ai.freeside.macverify.pf`
// on the reference host after the boot job's run, trimmed. The coalition
// blocks carry their own state lines, one tab deeper than the job's.
const launchctlPrintFinished = `system/ai.freeside.macverify.pf = {
	active count = 0
	path = /Library/LaunchDaemons/ai.freeside.macverify.pf.plist
	type = LaunchDaemon
	state = not running

	program = /sbin/pfctl
	domain = system
	minimum runtime = 10
	exit timeout = 5
	runs = 1
	last exit code = 0

	resource coalition = {
		ID = 121030
		type = resource
		state = active
		active count = 1
		name = ai.freeside.macverify.pf
	}

	job state = exited
	properties = runatload | inferred program
}
`

func TestParseLaunchctlPrint(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		out     string
		want    ServiceRun
		wantErr string
	}{
		"finished": {out: launchctlPrintFinished, want: ServiceRun{Runs: 1}},
		"failed": {
			out:  strings.Replace(launchctlPrintFinished, "last exit code = 0", "last exit code = 1", 1),
			want: ServiceRun{Runs: 1, LastExitCode: 1},
		},
		"running": {
			out:  strings.Replace(launchctlPrintFinished, "\tstate = not running", "\tstate = running", 1),
			want: ServiceRun{Running: true, Runs: 1},
		},
		"not yet run": {
			out:  strings.Replace(strings.Replace(launchctlPrintFinished, "\truns = 1\n", "", 1), "last exit code = 0", "last exit code = (never exited)", 1),
			want: ServiceRun{},
		},
		"no state": {
			out:     strings.Replace(launchctlPrintFinished, "\tstate = not running\n", "", 1),
			wantErr: "no state line",
		},
		"unreadable exit": {
			out:     strings.Replace(launchctlPrintFinished, "last exit code = 0", "last exit code = (never exited)", 1),
			wantErr: "last exit code",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLaunchctlPrint([]byte(tc.out))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parse = %+v, %v; want an error naming %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parse = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}
