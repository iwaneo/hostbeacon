// Package hugepages turns off transparent huge pages for the Agent.
package hugepages

import (
	"os"

	"golang.org/x/sys/unix"
)

// Disable turns off transparent huge pages for this process and the
// programs it starts. Where the kernel gives them to every process (THP
// "always", as on Debian), each 2 MB page holds a little of the Agent's
// memory, which took it past the resource budget (v1 spec §4.6).
//
// The Go runtime has used memory before main, so the first call starts the
// program again with the same arguments (same PID); in the new program the
// setting is on from the start, and Disable returns nil.
func Disable() error {
	disabled, err := unix.PrctlRetInt(unix.PR_GET_THP_DISABLE, 0, 0, 0, 0)
	if err != nil || disabled == 1 {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0); err != nil {
		return err
	}
	return unix.Exec("/proc/self/exe", os.Args, os.Environ())
}
