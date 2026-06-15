//go:build !darwin

package core

// inhibitSleep is a no-op on platforms without a known idle-sleep inhibitor.
// (movcaster's target is a MacBook casting to an LG webOS TV; see the darwin
// build for the real implementation.)
func inhibitSleep() func() { return func() {} }
