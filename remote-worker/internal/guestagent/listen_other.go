//go:build !linux

package guestagent

import (
	"fmt"
	"net"
	"strings"
)

// Listen supports only unix:<path> off Linux. vsock is a Linux-only transport (spec
// §5.1); this exists so the package — and a developer driving the agent locally over
// a Unix socket — builds and runs on a non-Linux workstation.
func Listen(addr string) (net.Listener, error) {
	scheme, rest, ok := strings.Cut(addr, ":")
	if !ok {
		return nil, fmt.Errorf("guestagent: listen address %q must be scheme:value", addr)
	}
	if scheme != "unix" {
		return nil, fmt.Errorf("guestagent: %q is Linux-only here; only unix:<path> is supported off Linux", scheme)
	}
	return net.Listen("unix", rest)
}
