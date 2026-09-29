//go:build !linux

package system

import "errors"

// Statfs is only on Linux; the Agent runs only there.
func Statfs(string) (FSSize, error) {
	return FSSize{}, errors.New("statfs is only on Linux")
}
