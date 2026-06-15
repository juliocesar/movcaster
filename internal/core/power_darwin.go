//go:build darwin

package core

import (
	"os"
	"os/exec"
	"strconv"
)

// inhibitSleep prevents macOS idle system sleep for the life of a cast, then
// returns a func that releases the assertion.
//
// Why: when the laptop's display sleeps, macOS otherwise lets the system go
// idle and throttles/suspends background work, so our HTTP server + ffmpeg stop
// feeding the TV. The stream stalls ("loading") a minute or two in and only
// resumes when the display wakes. `caffeinate -i` holds a
// PreventUserIdleSystemSleep assertion: the display still sleeps (we don't pass
// -d), only the idle stall is prevented. `-w <pid>` ties caffeinate to our
// process so it can never outlive movcaster, even on a crash.
func inhibitSleep() func() {
	cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
