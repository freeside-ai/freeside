package macverify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseToolchainOutput(t *testing.T) {
	t.Parallel()
	build, err := parseXcodeBuild("Xcode 27.2\nBuild version 27B52\n")
	if err != nil || build != "27B52" {
		t.Fatalf("parseXcodeBuild = %q, %v; want 27B52", build, err)
	}
	swift, err := parseSwiftToken("swift-driver version: 1.168.6 Apple Swift version 6.4.2 " +
		"(swiftlang-6.4.2.1.9 clang-2100.4.1.1)\nTarget: arm64-apple-macosx27.2.0\n")
	if err != nil || swift != "swiftlang-6.4.2.1.9" {
		t.Fatalf("parseSwiftToken = %q, %v; want swiftlang-6.4.2.1.9", swift, err)
	}
	if (Toolchain{XcodeBuild: build, Swift: swift}).validate() != nil {
		t.Fatal("parsed toolchain does not validate")
	}
	for name, out := range map[string]string{
		"no build line":  "Xcode 27.2\n",
		"two builds":     "Build version 27B52\nBuild version 27B53\n",
		"indented build": "  Build version 27B52\n",
	} {
		if got, err := parseXcodeBuild(out); err == nil {
			t.Errorf("%s: parseXcodeBuild = %q, want an error", name, got)
		}
	}
	for name, out := range map[string]string{
		"open-source toolchain":  "Swift version 6.1 (swift-6.1-RELEASE)\n",
		"two swiftlang versions": "(swiftlang-6.1 clang-1) (swiftlang-6.2 clang-2)\n",
	} {
		if got, err := parseSwiftToken(out); err == nil {
			t.Errorf("%s: parseSwiftToken = %q, want an error", name, got)
		}
	}
}

func TestRunRefusesAToolchainOtherThanThePin(t *testing.T) {
	t.Parallel()
	for name, host := range map[string]func(context.Context) (Toolchain, error){
		"other Xcode": func(context.Context) (Toolchain, error) {
			return Toolchain{XcodeBuild: "17F43", Swift: testToolchain.Swift}, nil
		},
		"other Swift": func(context.Context) (Toolchain, error) {
			return Toolchain{XcodeBuild: testToolchain.XcodeBuild, Swift: "swiftlang-6.1.2.1.3"}, nil
		},
		"unreadable": func(context.Context) (Toolchain, error) {
			return Toolchain{}, errors.New("xcrun: no developer tools")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			room := newTestRoom(t, fake)
			room.hostToolchain = host
			_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
			if err == nil {
				t.Fatal("Run accepted the host toolchain")
			}
			if name != "unreadable" && (!errors.Is(err, ErrToolchainMismatch) ||
				!strings.Contains(err.Error(), testToolchain.XcodeBuild)) {
				t.Fatalf("Run error = %v, want ErrToolchainMismatch naming the pin", err)
			}
			if calls, _, _ := fake.snapshot(); len(calls) != 0 {
				t.Fatalf("the room reached the helper: %v", calls)
			}
		})
	}
}
