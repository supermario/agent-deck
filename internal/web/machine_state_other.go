//go:build !darwin

package web

// Non-macOS builds can't read the Mac presence state, so never suppress: always
// report "wants push". The presence gate is a macOS-only convenience.
func machineWantsPush() (bool, string) {
	return true, "non-darwin"
}
