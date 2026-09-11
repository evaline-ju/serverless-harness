package guestagent

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AgentOptions configures the in-guest agent.
type AgentOptions struct {
	Shell      string // default "bash"
	WorkDir    string // the mounted workspace; every command's cwd
	ScratchDir string // guest tmpfs, for staged command files. Discarded with the VM
}

// Agent serves one command per connection out of a bash that was forked BEFORE the
// snapshot was taken and has been blocked on a pipe ever since.
//
// This is persistent-exec.ts's framing discipline reimplemented inside the guest —
// the fast channel P6 §3.1a records the gRPC path as lacking. It is safe here in a
// way it is not in a shared container: the VM serves exactly one command in its life
// and is then destroyed, so the parked shell has no cross-command state to leak
// (spec §5.4).
type Agent struct {
	opts AgentOptions

	mu      sync.Mutex // one command at a time: one VM, one Exec
	shell   *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  *bufio.Reader
	seq     uint64
	clockOK atomic.Bool
}

func NewAgent(opts AgentOptions) (*Agent, error) {
	if opts.Shell == "" {
		opts.Shell = "bash"
	}
	if opts.ScratchDir == "" {
		opts.ScratchDir = os.TempDir()
	}
	a := &Agent{opts: opts}
	if err := a.forkShell(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Agent) WorkDir() string { return a.opts.WorkDir }

// ShellPID is the parked shell's pid, so a test can prove it was not replaced.
func (a *Agent) ShellPID() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.shell == nil || a.shell.Process == nil {
		return 0
	}
	return a.shell.Process.Pid
}

// forkShell starts the shell that the snapshot captures blocked on its stdin pipe.
// The three pipes are created here rather than per command, because creating them per
// command is the fork this whole design removes.
func (a *Agent) forkShell() error {
	cmd := exec.Command(a.opts.Shell)
	cmd.Dir = a.opts.WorkDir
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	outR, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errR, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	a.mu.Lock()
	a.shell, a.stdin = cmd, in
	a.stdout, a.stderr = bufio.NewReaderSize(outR, MaxFrame), bufio.NewReaderSize(errR, MaxFrame)
	a.mu.Unlock()
	return nil
}

func (a *Agent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stdin != nil {
		_ = a.stdin.Close()
	}
	if a.shell != nil && a.shell.Process != nil {
		_ = a.shell.Process.Kill()
		_ = a.shell.Wait()
	}
	return nil
}

// Serve accepts connections forever. At snapshot time the agent is blocked HERE:
// Firecracker documents that listening vsock sockets survive restore with the CID
// updated, so the parked accept() is documented behaviour rather than a trick
// (spec §2.4, §5.1).
func (a *Agent) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() { _ = a.ServeConn(c) }()
	}
}

// ServeConn handles exactly one request on rw.
func (a *Agent) ServeConn(rw io.ReadWriteCloser) error {
	defer rw.Close()
	kind, payload, err := ReadFrame(rw)
	if err != nil {
		return err
	}
	if kind != KindRequest {
		return WriteFrame(rw, KindError, []byte(fmt.Sprintf("expected a request frame, got %#x", byte(kind))))
	}
	var req Request
	if err := DecodeJSON(payload, &req); err != nil {
		return WriteFrame(rw, KindError, []byte("undecodable request: "+err.Error()))
	}

	// Correct the wall clock before running anything: the guest resumed from the
	// snapshot moment, so `date`, file mtimes and git commit timestamps are all wrong
	// until this happens (spec §2.4, §5.3). Once per VM — the VM serves one command.
	if req.HostUnixNanos > 0 && !a.clockOK.Swap(true) {
		host := time.Unix(0, req.HostUnixNanos)
		if shouldSetClock(host, time.Now()) {
			if err := setWallClock(host); err != nil {
				// Not fatal: a wrong clock is a correctness problem for timestamps, not
				// a reason to fail the command. Reported on stderr so it is visible in
				// the run's own output rather than only in a host log.
				_ = WriteFrame(rw, KindStderr, []byte("guest-agent: clock: "+err.Error()+"\n"))
			}
		}
	}

	// The host ALWAYS sends KindStdinEOF, whether or not HasStdin is set — draining
	// only conditionally would desynchronise the stream on every no-stdin command.
	stdin, err := a.readStdin(rw)
	if err != nil {
		return WriteFrame(rw, KindError, []byte("reading stdin: "+err.Error()))
	}

	capBytes := req.CapBytes
	if capBytes <= 0 {
		capBytes = MaxFrame
	}
	w := &frameWriter{rw: rw, cap: capBytes}

	var end End
	if req.HasStdin {
		// Spec §5.4's one thing the parked shell cannot do: `base64 -d > file` only
		// terminates at EOF, and you cannot close a long-lived shell's stdin without
		// killing it. So a stdin command gets a freshly forked child.
		end, err = a.runFreshChild(req, stdin, w)
	} else {
		end, err = a.runOnParkedShell(req, w)
	}
	if err != nil {
		return WriteFrame(rw, KindError, []byte(err.Error()))
	}
	end.DroppedStdout, end.DroppedStderr = w.droppedStdout, w.droppedStderr
	return WriteJSON(rw, KindEnd, end)
}

// readStdin drains stdin frames until KindStdinEOF, regardless of whether the request
// claimed HasStdin. The host always sends the EOF frame; an agent that skipped this
// read for a no-stdin command would leave that frame sitting in the connection and
// desynchronise everything after it.
func (a *Agent) readStdin(rw io.ReadWriteCloser) ([]byte, error) {
	var buf []byte
	for {
		kind, payload, err := ReadFrame(rw)
		if err != nil {
			return nil, err
		}
		switch kind {
		case KindStdin:
			buf = append(buf, payload...)
		case KindStdinEOF:
			return buf, nil
		default:
			return nil, fmt.Errorf("unexpected frame %#x while reading stdin", byte(kind))
		}
	}
}

// runOnParkedShell stages the command as a file and sources it, then reads until a
// per-command nonce appears on BOTH streams.
//
// Staged as a file rather than written line-by-line into the shell's stdin because a
// heredoc, a multi-line command or an unbalanced quote would otherwise wedge the
// parked shell — and agent-authored bash is exactly where those arrive. The file lives
// in guest tmpfs and dies with the VM.
//
// The nonce goes to stderr too. stdout's nonce says when the command finished, but
// stderr has no such delimiter, so a drain that stopped at stdout's nonce would race
// with trailing stderr and drop it.
func (a *Agent) runOnParkedShell(req Request, w *frameWriter) (End, error) {
	a.mu.Lock()
	a.seq++
	seq := a.seq
	stdin, stdout, stderr := a.stdin, a.stdout, a.stderr
	a.mu.Unlock()

	nonce, err := newNonce()
	if err != nil {
		return End{}, err
	}
	path := filepath.Join(a.opts.ScratchDir, fmt.Sprintf("ga-cmd-%d", seq))
	if err := os.WriteFile(path, []byte(req.Command), 0o600); err != nil {
		return End{}, fmt.Errorf("staging command: %w", err)
	}
	defer os.Remove(path)

	// The sourced file runs inside a subshell "( ... )" rather than bare ". path":
	// bash's `exit` inside a plain source terminates the CURRENT shell, which would
	// kill the parked shell on the very first `exit` a command issues. A subshell
	// only forks the already-resident bash image (no exec, no re-parsing of a shell
	// binary) — it is not the fork this design removes, which is starting bash from
	// scratch. "$?" after the subshell closes is the sourced script's exit status.
	//
	// No leading newline before the nonce: the printf write and the nonce line are
	// read as separate lines by drainUntilNonce, so a leading "\n" here would surface
	// as a spurious blank line of "output" ahead of the sentinel on every command.
	line := fmt.Sprintf("( . %q )\n__ga_rc=$?\nprintf '%s %%d\\n' \"$__ga_rc\"\nprintf '%s\\n' >&2\n", path, nonce, nonce)
	if _, err := io.WriteString(stdin, line); err != nil {
		return End{}, fmt.Errorf("writing to the parked shell: %w", err)
	}

	type drainResult struct {
		code int32
		err  error
	}
	outDone := make(chan drainResult, 1)
	errDone := make(chan error, 1)
	go func() {
		code, err := drainUntilNonce(stdout, nonce, w.stdout, true)
		outDone <- drainResult{code, err}
	}()
	go func() {
		_, err := drainUntilNonce(stderr, nonce, w.stderr, false)
		errDone <- err
	}()

	var timeout <-chan time.Time
	if req.TimeoutS > 0 {
		tm := time.NewTimer(time.Duration(req.TimeoutS) * time.Second)
		defer tm.Stop()
		timeout = tm.C
	}
	var res drainResult
	select {
	case res = <-outDone:
	case <-timeout:
		// The host's destroy is the real enforcement — teardown is the same path as
		// success (spec §4.1). This exists so a longer host timeout still yields an
		// explicit frame rather than silence.
		return End{}, fmt.Errorf("timeout:%d", req.TimeoutS)
	}
	if res.err != nil {
		return End{}, res.err
	}
	select {
	case <-errDone:
	case <-time.After(time.Second):
		// stderr's nonce did not arrive within a second of stdout's. Report what we
		// have rather than hanging: the VM is about to be destroyed anyway.
	}
	return End{ExitCode: res.code}, nil
}

// runFreshChild is the stdin path: a real bash -c child whose stdin is fed and closed.
func (a *Agent) runFreshChild(req Request, stdin []byte, w *frameWriter) (End, error) {
	cmd := exec.Command(a.opts.Shell, "-c", req.Command)
	cmd.Dir = a.opts.WorkDir
	cmd.Stdout = w.stdoutWriter()
	cmd.Stderr = w.stderrWriter()
	in, err := cmd.StdinPipe()
	if err != nil {
		return End{}, err
	}
	if err := cmd.Start(); err != nil {
		return End{}, err
	}
	go func() {
		_, _ = in.Write(stdin)
		_ = in.Close() // the EOF `base64 -d > file` waits for
	}()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var timeout <-chan time.Time
	if req.TimeoutS > 0 {
		tm := time.NewTimer(time.Duration(req.TimeoutS) * time.Second)
		defer tm.Stop()
		timeout = tm.C
	}
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			return End{ExitCode: int32(ee.ExitCode())}, nil
		}
		if err != nil {
			return End{}, err
		}
		return End{ExitCode: 0}, nil
	case <-timeout:
		_ = cmd.Process.Kill()
		return End{}, fmt.Errorf("timeout:%d", req.TimeoutS)
	}
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "__GA_" + hex.EncodeToString(b[:]), nil
}

// shouldSetClock decides whether the guest's wall clock is far enough out to correct.
// One second, because the trap is not drift: the guest resumes from the SNAPSHOT
// moment, so the error is however long the golden snapshot has been in service —
// hours, not milliseconds (spec §2.4). A tight threshold would call settimeofday on
// every Exec for no benefit.
func shouldSetClock(host, guest time.Time) bool {
	d := host.Sub(guest)
	if d < 0 {
		d = -d
	}
	return d > time.Second
}

// frameWriter forwards output as frames, capping EACH stream independently and
// counting what it threw away.
//
// Capping here rather than with `head -c` in the guest pipeline is deliberate: `head`
// in a pipeline changes the command's exit status via SIGPIPE, and the exit code is
// the one thing a worker must report faithfully. Same byte saving, no perturbation.
type frameWriter struct {
	rw            io.Writer
	cap           int64
	mu            sync.Mutex
	sentStdout    int64
	sentStderr    int64
	droppedStdout int64
	droppedStderr int64
}

func (w *frameWriter) stdout(b []byte) { w.emit(KindStdout, b, &w.sentStdout, &w.droppedStdout) }

func (w *frameWriter) stderr(b []byte) { w.emit(KindStderr, b, &w.sentStderr, &w.droppedStderr) }

func (w *frameWriter) emit(k Kind, b []byte, sent, dropped *int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.cap - *sent
	if room <= 0 {
		*dropped += int64(len(b))
		return
	}
	if int64(len(b)) > room {
		*dropped += int64(len(b)) - room
		b = b[:room]
	}
	*sent += int64(len(b))
	_ = WriteFrame(w.rw, k, b)
}

func (w *frameWriter) stdoutWriter() io.Writer { return writerFunc(w.stdout) }
func (w *frameWriter) stderrWriter() io.Writer { return writerFunc(w.stderr) }

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

// drainUntilNonce forwards bytes to emit until it sees the nonce line, returning the
// exit code that line carries (stdout only). It reads line-wise because the nonce is
// line-delimited, and forwards in MaxFrame slices so one long line still streams.
func drainUntilNonce(r *bufio.Reader, nonce string, emit func([]byte), wantCode bool) (int32, error) {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if idx := indexNonce(line, nonce); idx >= 0 {
				if idx > 0 {
					emitChunked(emit, line[:idx])
				}
				if !wantCode {
					return 0, nil
				}
				return parseNonceCode(line[idx:], nonce)
			}
			emitChunked(emit, line)
		}
		if err != nil {
			// The shell died before the nonce: report it rather than inventing a code.
			return 0, fmt.Errorf("parked shell ended before its sentinel: %w", err)
		}
	}
}

// indexNonce returns the byte offset of nonce within line, or -1 if absent.
func indexNonce(line []byte, nonce string) int {
	return bytes.Index(line, []byte(nonce))
}

// parseNonceCode parses the "<nonce> <code>\n" suffix that runOnParkedShell writes
// after the nonce on stdout. It returns an error rather than a zero exit code when it
// cannot parse: inventing a zero would report success for a command whose actual
// status is unknown.
func parseNonceCode(tail []byte, nonce string) (int32, error) {
	s := strings.TrimRight(string(tail), "\n")
	s = strings.TrimPrefix(s, nonce)
	s = strings.TrimSpace(s)
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing exit code from sentinel line %q: %w", tail, err)
	}
	return int32(n), nil
}

// emitChunked slices b into MaxFrame pieces before calling emit, so one long line
// (e.g. from a command with no newlines at all) still streams instead of arriving as
// one oversized write that frameWriter would have to further slice itself.
func emitChunked(emit func([]byte), b []byte) {
	for off := 0; off < len(b); off += MaxFrame {
		end := off + MaxFrame
		if end > len(b) {
			end = len(b)
		}
		emit(b[off:end])
	}
}
