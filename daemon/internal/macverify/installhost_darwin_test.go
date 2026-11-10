//go:build darwin

package macverify

import "testing"

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
