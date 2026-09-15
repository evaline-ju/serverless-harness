package vmpool

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

// dialVsock performs Firecracker's (and Cloud Hypervisor's — both share this shape)
// host-initiated vsock handshake and returns the resulting duplex stream.
//
// path is the vsock device's own uds_path, dialed directly with NO port suffix. That
// is deliberate, not an oversight: Firecracker's docs (docs/vsock.md, "Host-Initiated
// Connections") describe a HOST-initiated connection — which is what a launcher
// running commands in an already-booted guest always does — as connecting straight to
// uds_path, writing "CONNECT <port>\n", and reading an "OK <port>\n" acknowledgement.
// The suffixed form "<uds_path>_<port>" is real, but it names the socket Firecracker
// creates for the OPPOSITE direction (a connection the GUEST initiates outward); using
// it here would dial a socket nothing is listening on. This matches the only other
// implementation of this handshake in the repo, deploy/microvm/build-snapshot.sh's
// guest_client.go, which also dials the bare path.
func dialVsock(path string, port uint32) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", path, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("vsock: dial %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock: send CONNECT %d: %w", port, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock: read CONNECT ack: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("vsock: CONNECT %d refused: %q", port, strings.TrimSuffix(line, "\n"))
	}
	_ = conn.SetReadDeadline(time.Time{})
	// The bufio.Reader above may have buffered bytes past the "OK ...\n" line — the
	// guest's first protocol frame, if it answered fast enough to already be in
	// flight. Handing back conn directly would silently drop those bytes; wrap it so
	// reads first drain what br already buffered.
	return &handshakeConn{Conn: conn, buffered: br}, nil
}

// handshakeConn hands back bytes the handshake's bufio.Reader read past the "OK" line.
// Without this the caller's first ReadFrame can silently lose the guest's first frame —
// a bug that only shows up under load, when the guest answers fast enough to have its
// reply already in flight.
type handshakeConn struct {
	net.Conn
	buffered *bufio.Reader
}

func (c *handshakeConn) Read(p []byte) (int, error) { return c.buffered.Read(p) }
