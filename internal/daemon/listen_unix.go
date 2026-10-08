//go:build unix

package daemon

import (
	"fmt"
	"net"
)

// listenSocket (Unix) binds the daemon's unix domain socket at path.
// Go's net package unlinks the socket file when the listener closes,
// so the explicit cleanup in Server.removeArtifacts is a belt-and-
// suspenders second pass, not the primary unlink.
func listenSocket(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on %s: %w", path, err)
	}
	return ln, nil
}
