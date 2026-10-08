//go:build windows

package daemon

import (
	"fmt"
	"net"
)

// listenSocket (Windows) always fails with [ErrUnsupportedPlatform].
// Windows binaries still cross-compile the full daemon package (the
// discovery probe and protocol types are shared with the CLI and shim
// surfaces), but serving over a unix domain socket is gated to unix
// hosts until a Windows transport is specified.
func listenSocket(path string) (net.Listener, error) {
	return nil, fmt.Errorf("daemon: listen on %s: %w", path, ErrUnsupportedPlatform)
}
