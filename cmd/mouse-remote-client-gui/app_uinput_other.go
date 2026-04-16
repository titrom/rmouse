//go:build !linux

package main

// On Windows / macOS the injector backends don't need a one-time
// privilege grant, so these are no-ops that report "all good".
func hasUinputAccess() bool      { return true }
func requestUinputAccess() error { return nil }
