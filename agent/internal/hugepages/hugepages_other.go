//go:build !linux

package hugepages

// Disable does nothing: transparent huge pages are Linux only.
func Disable() error { return nil }
