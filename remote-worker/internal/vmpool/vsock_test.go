package vmpool

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Firecracker's (and Cloud Hypervisor's) host-side vsock backend is a single Unix
// socket at the path given as uds_path: for a HOST-INITIATED connection (which is our
// case — the host dials INTO the guest agent's listening vsock port), the host
// connects directly to that path, writes "CONNECT <port>\n", and reads "OK <port>\n"
// back (docs/vsock.md, "Host-Initiated Connections"; verified against
// github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md since this
// cannot be exercised without a hypervisor).
//
// The brief's original draft of this test had dialVsock dial "<uds_path>_<port>".
// That suffixed form is real, but it is the address Firecracker uses for the OPPOSITE
// direction: a connection the GUEST initiates outward, which the host observes
// appearing at "<uds_path>_<port>" (one such socket per guest-opened port). It is not
// what a host-initiated CONNECT dials, and deploy/microvm/build-snapshot.sh's own
// guest_client.go (the only other place in this repo that speaks this handshake)
// already dials the bare uds_path with no suffix — this test, and dialVsock below,
// are corrected to match that and the upstream docs.
func TestDialVsockCompletesTheConnectHandshake(t *testing.T) {
	// shortUnixSocketDir (fcapi_test.go): t.TempDir() nests too deep for AF_UNIX on
	// macOS.
	path := filepath.Join(shortUnixSocketDir(t), "v.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(path) })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		if line != "CONNECT 1024\n" {
			_, _ = c.Write([]byte("ERR\n"))
			return
		}
		_, _ = c.Write([]byte("OK 12345\n"))
		_, _ = c.Write([]byte("payload"))
	}()

	conn, err := dialVsock(path, 1024)
	if err != nil {
		t.Fatalf("dialVsock: %v", err)
	}
	defer conn.Close()
	// The handshake must be fully consumed before the caller reads, or the first
	// protocol frame the caller sees is the tail of "OK 12345\n".
	buf := make([]byte, 7)
	if _, err := conn.Read(buf); err != nil || string(buf) != "payload" {
		t.Fatalf("first bytes after the handshake = %q err=%v", buf, err)
	}
}

func TestDialVsockFailsOnARefusedConnect(t *testing.T) {
	path := filepath.Join(shortUnixSocketDir(t), "v.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, _ := ln.Accept()
		if c != nil {
			_, _ = c.Write([]byte("ERR\n"))
			_ = c.Close()
		}
	}()
	if _, err := dialVsock(path, 1024); err == nil {
		t.Fatal("dialVsock ignored a refused CONNECT")
	}
}
