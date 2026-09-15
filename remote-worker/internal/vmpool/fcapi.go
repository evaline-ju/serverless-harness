package vmpool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// fcClient talks to Firecracker's HTTP Management API over the unix socket the
// jailer/firecracker process listens on. Every request goes over that socket
// regardless of the URL's host, so the transport ignores the dialed address.
type fcClient struct {
	sock string
	http *http.Client

	// vsockOverride, when non-empty, is sent as vsock_override on /snapshot/load. The
	// golden snapshot's vsock device was configured (by deploy/microvm/build-snapshot.sh)
	// with a uds_path that was an absolute path inside that build's own throwaway
	// staging directory — a path this launcher has no way to know or reproduce, and
	// one the manifest (snapshot.go's Manifest) does not record. Firecracker's
	// snapshot/load API exists to solve exactly this: it lets the restorer redirect the
	// vsock backend to a path of ITS choosing without touching the snapshot file
	// (docs/vsock.md, "Unix Domain Socket Renaming"). Restore sets this to a fixed
	// relative name inside the VM's own jail before calling LoadSnapshot. On the wire
	// this is an object, not a bare string — see loadSnapshotRequest.VsockOverride.
	vsockOverride string
}

// newFCClient builds a client bound to a Firecracker (or Cloud Hypervisor, which
// serves a compatible enough subset for these two calls) API socket.
func newFCClient(sock string) *fcClient {
	return &fcClient{
		sock: sock,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
			},
		},
	}
}

// setVsockOverride records the path Restore wants LoadSnapshot to redirect the vsock
// backend to. Package-internal only: production callers reach it through
// firecrackerVM.Resume (launcher_firecracker.go); the API-client tests in
// fcapi_test.go set it directly to assert on the serialized wire shape.
func (c *fcClient) setVsockOverride(path string) { c.vsockOverride = path }

type loadSnapshotRequest struct {
	SnapshotPath string `json:"snapshot_path"`
	MemBackend   struct {
		BackendPath string `json:"backend_path"`
		BackendType string `json:"backend_type"`
	} `json:"mem_backend"`
	// EnableDiffSnapshots must stay false: incremental ("diff") snapshots are a
	// developer-preview feature and generally not resumable (spec §2.4) — there is no
	// incremental strategy for this arm, only full snapshots.
	EnableDiffSnapshots bool `json:"enable_diff_snapshots"`
	// ResumeVM must stay false: standbys are restored PAUSED, not running. A paused VM
	// costs zero CPU, which is what lets many standbys exist without burning cores on
	// timer ticks (spec §3.2) — resuming here would make every standby a running VM.
	ResumeVM bool `json:"resume_vm"`
	// VsockOverride, when set, redirects the vsock device's host-side Unix socket to a
	// path of the restorer's choosing. See fcClient.vsockOverride's comment for why.
	//
	// Wire shape: an OBJECT with a single "uds_path" member — e.g.
	// {"vsock_override":{"uds_path":"/vsock.sock"}} — NOT a bare string. Firecracker
	// v1.17.0 rejects "vsock_override":"<path>" with "invalid type: string ..., expected
	// struct VsockOverride"; do not "simplify" this back to a plain string. This must
	// agree with the shape deploy/microvm/build-snapshot.sh sends to /snapshot/load.
	//
	// The field is a pointer so that omitempty actually omits it when no override is
	// set: Firecracker accepts an absent vsock_override but rejects an empty object
	// ({}) with "missing field `uds_path`", so absence — not {} — is the only valid
	// "no override" wire form, and a nil *struct is what omitempty treats as empty.
	VsockOverride *struct {
		UDSPath string `json:"uds_path"`
	} `json:"vsock_override,omitempty"`
}

// LoadSnapshot restores a VM from vmstate/memfile. vmstate and memfile are paths AS
// SEEN BY FIRECRACKER — i.e. inside its jail, not host paths — since the process is
// chrooted by the jailer by the time this is called.
func (c *fcClient) LoadSnapshot(ctx context.Context, vmstate, memfile string) error {
	req := loadSnapshotRequest{SnapshotPath: vmstate}
	req.MemBackend.BackendPath = memfile
	req.MemBackend.BackendType = "File"
	if c.vsockOverride != "" {
		req.VsockOverride = &struct {
			UDSPath string `json:"uds_path"`
		}{UDSPath: c.vsockOverride}
	}
	return c.do(ctx, http.MethodPut, "/snapshot/load", req)
}

// Resume flips a paused VM to running. It does nothing else — no mount, no vsock
// dial — those happen in firecrackerVM.Resume (launcher_firecracker.go), which calls
// this first and then immediately mounts the workspace over its own fresh vsock
// connection (mount-at-acquire, spec §4.3; see launcher.go's VM.Resume doc comment,
// the authoritative source once it and this method's mount timing disagreed).
func (c *fcClient) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", struct {
		State string `json:"state"`
	}{State: "Resumed"})
}

// fcFault is the shape of Firecracker's JSON error body.
type fcFault struct {
	FaultMessage string `json:"fault_message"`
}

func (c *fcClient) do(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("fcapi: encode %s %s: %w", method, path, err)
	}
	// The URL's host is ignored by the DialContext override above; "localhost" is a
	// placeholder to keep net/http happy about having one.
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("fcapi: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fcapi: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		// The fault_message must survive verbatim: "restore failed" without it is
		// undiagnosable, and spec §6 treats a CRC failure as FATAL FOR THE HOST — a
		// decision that needs the VMM's own stated reason, not just an HTTP code.
		var fault fcFault
		if err := json.Unmarshal(respBody, &fault); err == nil && fault.FaultMessage != "" {
			return fmt.Errorf("fcapi: %s %s: %s (status %d)", method, path, fault.FaultMessage, resp.StatusCode)
		}
		return fmt.Errorf("fcapi: %s %s: status %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	return nil
}
