//go:build linux

package guestagent

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Listen parses addr as "vsock:<port>" or "unix:<path>" and returns a bound listener.
// unix: is kept alongside vsock: so the same binary can be driven over a local socket
// during development without needing a VM (spec §5.1's transport is vsock only once
// inside a real guest).
func Listen(addr string) (net.Listener, error) {
	scheme, rest, ok := strings.Cut(addr, ":")
	if !ok {
		return nil, fmt.Errorf("guestagent: listen address %q must be scheme:value", addr)
	}
	switch scheme {
	case "vsock":
		port, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("guestagent: vsock port %q: %w", rest, err)
		}
		return listenVsock(uint32(port))
	case "unix":
		return net.Listen("unix", rest)
	default:
		return nil, fmt.Errorf("guestagent: unknown listen scheme %q", scheme)
	}
}

// listenVsock binds AF_VSOCK on VMADDR_CID_ANY: inside the guest there is exactly one
// vsock device, and the host's CID can vary (it changes across a restore per spec
// §2.4), so the guest side has no fixed peer CID to bind to — only a port.
func listenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("guestagent: vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("guestagent: vsock bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("guestagent: vsock listen port %d: %w", port, err)
	}
	// os.NewFile takes ownership of fd for os.File's purposes; net.FileListener dups
	// it for the runtime poller, so f is closed once the dup has been taken.
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port))
	defer f.Close()
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("guestagent: vsock FileListener port %d: %w", port, err)
	}
	return ln, nil
}
