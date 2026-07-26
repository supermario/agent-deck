//go:build darwin

package web

// Native (cgo) probe of the Mac's presence state, used to gate proactive push
// notifications: we only want to buzz the phone when the user has stepped away.
//
//   - screen locked        -> away  (walked off, screen locked)
//   - lid closed / clamshell -> away (docked or in a bag)
//   - unlocked + lid open   -> at the desk, actively working -> suppress push
//
// No process spawn: reads CGSessionCopyCurrentDictionary (CoreGraphics) and the
// IOPMrootDomain's AppleClamshellState (IOKit) directly, so it's instant.

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation -framework IOKit
#include <CoreGraphics/CoreGraphics.h>
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>

// 1 if the login session's screen is locked, 0 otherwise.
static int md_screenLocked() {
    CFDictionaryRef d = CGSessionCopyCurrentDictionary();
    if (!d) return 0;
    int locked = 0;
    CFBooleanRef v = (CFBooleanRef)CFDictionaryGetValue(d, CFSTR("CGSSessionScreenIsLocked"));
    if (v && CFGetTypeID(v) == CFBooleanGetTypeID()) locked = CFBooleanGetValue(v) ? 1 : 0;
    CFRelease(d);
    return locked;
}

// 1 if the lid is closed (clamshell), 0 if open, -1 if unknown (e.g. desktop Mac).
static int md_clamshellClosed() {
    io_service_t pmRoot = IOServiceGetMatchingService(kIOMainPortDefault, IOServiceMatching("IOPMrootDomain"));
    if (!pmRoot) return -1;
    CFBooleanRef v = (CFBooleanRef)IORegistryEntryCreateCFProperty(pmRoot, CFSTR("AppleClamshellState"), kCFAllocatorDefault, 0);
    int closed = -1;
    if (v && CFGetTypeID(v) == CFBooleanGetTypeID()) closed = CFBooleanGetValue(v) ? 1 : 0;
    if (v) CFRelease(v);
    IOObjectRelease(pmRoot);
    return closed;
}
*/
import "C"

// machineWantsPush reports whether the Mac is in an "away" state where the user
// wants proactive push notifications (screen locked, or lid closed/clamshell).
// When unlocked with the lid open, the user is at the desk and pushes are
// suppressed. The reason string is for logging.
func machineWantsPush() (bool, string) {
	if int(C.md_screenLocked()) == 1 {
		return true, "locked"
	}
	if int(C.md_clamshellClosed()) == 1 {
		return true, "clamshell"
	}
	return false, "unlocked+open"
}
