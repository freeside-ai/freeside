//go:build darwin && cgo

package cordon

/*
#cgo LDFLAGS: -framework CoreGraphics -framework ApplicationServices -framework Security
#include <CoreGraphics/CoreGraphics.h>
#include <ApplicationServices/ApplicationServices.h>
#include <Security/AuthSession.h>

// Neither preflight prompts. Each answers for this process's responsible
// process: under a LaunchAgent, the agent's own program.
static int cordon_screen_recording(void) { return CGPreflightScreenCaptureAccess() ? 1 : 0; }
static int cordon_accessibility(void) { return AXIsProcessTrusted() ? 1 : 0; }

static int cordon_desktop_session(int *gui) {
	SecuritySessionId id;
	SessionAttributeBits bits;
	OSStatus status = SessionGetInfo(callerSecuritySession, &id, &bits);
	if (status != errSessionSuccess) {
		return (int)status;
	}
	*gui = (bits & sessionHasGraphicAccess) ? 1 : 0;
	return 0;
}
*/
import "C"

import "fmt"

// readHostFacts asks TCC about the two approvals and the security server
// about this process's session.
func readHostFacts() (HostFacts, error) {
	var gui C.int
	if status := C.cordon_desktop_session(&gui); status != 0 {
		return HostFacts{}, fmt.Errorf("SessionGetInfo: OSStatus %d", int(status))
	}
	return HostFacts{
		ScreenRecording: C.cordon_screen_recording() == 1,
		Accessibility:   C.cordon_accessibility() == 1,
		DesktopSession:  gui == 1,
	}, nil
}
