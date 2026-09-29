//go:build !linux && !darwin

package helper

import (
	"errors"
	"net"
)

// peerUID cannot tell the caller here, so every caller is refused.
func peerUID(*net.UnixConn) (int, error) {
	return 0, errors.New("cannot read the caller's user on this system")
}
