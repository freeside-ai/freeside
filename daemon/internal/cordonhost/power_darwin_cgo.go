//go:build darwin && cgo

package cordonhost

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/pwr_mgt/IOPMLib.h>

static IOReturn cordonhost_prevent_idle_sleep(IOPMAssertionID *id) {
	CFStringRef reason = CFSTR("Freeside is running a cordoned launch");
	return IOPMAssertionCreateWithName(kIOPMAssertionTypePreventUserIdleSystemSleep,
		kIOPMAssertionLevelOn, reason, id);
}
*/
import "C"

import "fmt"

// preventIdleSleep takes a power assertion against idle system sleep and
// returns its release.
func preventIdleSleep() (func() error, error) {
	var id C.IOPMAssertionID
	if ret := C.cordonhost_prevent_idle_sleep(&id); ret != C.kIOReturnSuccess {
		return nil, fmt.Errorf("IOPMAssertionCreateWithName returned %#x", uint32(ret))
	}
	return func() error {
		if ret := C.IOPMAssertionRelease(id); ret != C.kIOReturnSuccess {
			return fmt.Errorf("IOPMAssertionRelease returned %#x", uint32(ret))
		}
		return nil
	}, nil
}
