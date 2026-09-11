package guestagent

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// drive runs one request against a real Agent over a net.Pipe and collects the reply.
func drive(t *testing.T, a *Agent, req Request, stdin []byte) (stdout, stderr string, end End, gotErr string) {
	t.Helper()
	host, guest := net.Pipe()
	go func() { _ = a.ServeConn(guest) }()
	defer host.Close()

	if err := WriteJSON(host, KindRequest, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	for off := 0; off < len(stdin); off += MaxFrame {
		e := off + MaxFrame
		if e > len(stdin) {
			e = len(stdin)
		}
		if err := WriteFrame(host, KindStdin, stdin[off:e]); err != nil {
			t.Fatalf("write stdin: %v", err)
		}
	}
	if err := WriteFrame(host, KindStdinEOF, nil); err != nil {
		t.Fatalf("write stdin EOF: %v", err)
	}
	var sb, eb strings.Builder
	for {
		k, p, err := ReadFrame(host)
		if err == io.EOF {
			t.Fatal("agent closed without End or Error")
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		switch k {
		case KindStdout:
			sb.Write(p)
		case KindStderr:
			eb.Write(p)
		case KindEnd:
			if err := DecodeJSON(p, &end); err != nil {
				t.Fatalf("decode End: %v", err)
			}
			return sb.String(), eb.String(), end, ""
		case KindError:
			return sb.String(), eb.String(), End{}, string(p)
		}
	}
}

func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	work := t.TempDir()
	a, err := NewAgent(AgentOptions{WorkDir: work, ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestAgentRunsOnTheParkedShellWithoutForking(t *testing.T) {
	a := newTestAgent(t)
	pid := a.ShellPID()
	if pid <= 0 {
		t.Fatal("ShellPID = 0: the shell must be forked at construction, which is what the snapshot captures")
	}
	stdout, stderr, end, errMsg := drive(t, a, Request{
		Command: "echo hi; echo oops >&2; exit 7", CapBytes: 1 << 20, HostUnixNanos: time.Now().UnixNano(),
	}, nil)
	if errMsg != "" {
		t.Fatalf("agent error: %s", errMsg)
	}
	if stdout != "hi\n" || stderr != "oops\n" || end.ExitCode != 7 {
		t.Fatalf("stdout=%q stderr=%q end=%+v", stdout, stderr, end)
	}
	// The whole point of §5.4: no fork. Same shell across commands, so the dominant
	// hot-path term — starting bash — is not paid per Exec.
	if _, _, end2, _ := drive(t, a, Request{Command: "echo again", CapBytes: 1 << 20}, nil); end2.ExitCode != 0 {
		t.Fatalf("second command: end=%+v", end2)
	}
	if a.ShellPID() != pid {
		t.Fatalf("ShellPID changed %d -> %d: the parked shell was replaced", pid, a.ShellPID())
	}
}

func TestAgentHandlesMultilineAndQuotedCommands(t *testing.T) {
	a := newTestAgent(t)
	// A command written line-by-line into a shell's stdin would break on either of
	// these. The agent stages it as a file and sources it instead.
	stdout, _, end, errMsg := drive(t, a, Request{
		Command:  "cat <<'EOF'\nline one\nline 'two'\nEOF",
		CapBytes: 1 << 20,
	}, nil)
	if errMsg != "" || end.ExitCode != 0 {
		t.Fatalf("end=%+v err=%s", end, errMsg)
	}
	if stdout != "line one\nline 'two'\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

// Spec §5.4's documented limitation: a command that consumes stdin needs a freshly
// forked child, because you cannot close a long-lived parked shell's stdin without
// killing it.
func TestAgentServesAWriteWithStdinOnAFreshChild(t *testing.T) {
	a := newTestAgent(t)
	pid := a.ShellPID()
	content := "hello from the harness\n"
	stdin := []byte(base64.StdEncoding.EncodeToString([]byte(content)))
	_, stderr, end, errMsg := drive(t, a, Request{
		Command: "base64 -d > written.txt", HasStdin: true, CapBytes: 1 << 20,
	}, stdin)
	if errMsg != "" || end.ExitCode != 0 {
		t.Fatalf("end=%+v stderr=%q err=%s", end, stderr, errMsg)
	}
	b, err := os.ReadFile(filepath.Join(a.WorkDir(), "written.txt"))
	if err != nil || string(b) != content {
		t.Fatalf("written.txt = %q err=%v", b, err)
	}
	// The parked shell survived: the stdin command went to a child, not to it.
	if a.ShellPID() != pid {
		t.Fatalf("ShellPID changed %d -> %d — feeding stdin must not kill the parked shell", pid, a.ShellPID())
	}
}

func TestAgentCapsEachStreamAtSourceAndReportsDropped(t *testing.T) {
	a := newTestAgent(t)
	stdout, stderr, end, errMsg := drive(t, a, Request{
		// 4096 bytes on each stream against a 100-byte cap.
		Command:  "head -c 4096 /dev/zero | tr '\\0' 'a'; head -c 4096 /dev/zero | tr '\\0' 'b' >&2",
		CapBytes: 100,
	}, nil)
	if errMsg != "" {
		t.Fatalf("agent error: %s", errMsg)
	}
	if len(stdout) != 100 || len(stderr) != 100 {
		t.Fatalf("forwarded %d stdout and %d stderr bytes, want 100 each — spec §4.1 prefers "+
			"capping at source over moving 8 MiB across vsock and discarding it", len(stdout), len(stderr))
	}
	if end.DroppedStdout != 4096-100 || end.DroppedStderr != 4096-100 {
		t.Fatalf("end = %+v, want ~3996 dropped on each stream, counted separately", end)
	}
	// And the exit code is the COMMAND's, not a truncation artifact: capping in the
	// agent rather than with `head -c` in the pipeline is what preserves it.
	if end.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", end.ExitCode)
	}
}

func TestAgentReportsATimeoutRatherThanHanging(t *testing.T) {
	a := newTestAgent(t)
	_, _, _, errMsg := drive(t, a, Request{Command: "sleep 30", TimeoutS: 1, CapBytes: 1 << 20}, nil)
	// The host's destroy is the real enforcement (spec §4.1: teardown is the same path
	// as success). This is belt-and-braces so a longer host timeout still gets an
	// explicit frame instead of silence.
	if !strings.HasPrefix(errMsg, "timeout:") {
		t.Fatalf("agent error = %q, want a timeout: prefix", errMsg)
	}
}

// TestAgentCapsEachStreamAtSourceAndReportsDropped above drives 4096 bytes against
// the 32 KiB MaxFrame read buffer, so the whole unterminated line fits in a single
// drainUntilNonce Read() call and the carry-over path added in fix round 1 is never
// taken — that test would pass identically against the pre-rewrite ReadBytes('\n')
// implementation, which is exactly the regression this test guards against.
//
// 160 KiB is five times MaxFrame, forcing drainUntilNonce through several Read()
// cycles on one line with no newline at all, carrying a small remainder across each
// boundary. The exact byte counts (not just "capped, roughly") are what would catch
// an off-by-one in that carry-over, which is the one way the rewrite could go subtly
// wrong.
// TestAgentBoundsDrainAcrossMultipleReadCycles above forces several Read() cycles,
// but its sentinel is written as a separate small printf, so the sentinel always
// lands wholly inside one 32 KiB read: the boundary-spanning case the carry-over
// exists for is never produced. A revert to ReadBytes('\n') would also pass it,
// since buffering the whole line and then capping yields identical observable
// output and dropped counts -- only memory use differs, which a test cannot easily
// see. (Both found by mutation-testing fix round 2: setting the carry-over to 0
// left that test, and the cap test, green.)
//
// This test drives drainUntilNonce directly instead of through a real subprocess
// pipe, because a subprocess pipe gives no way to guarantee which byte lands in
// which Read() call -- the whole point here is putting the split *inside* the
// nonce, not just forcing "a few" reads somewhere. An io.Pipe gives that guarantee:
// bufio.Reader forwards a Read() whose buffer is exactly the bufio's own buffer
// size (MaxFrame, matching how Agent builds a.stdout/a.stderr) straight through to
// the underlying reader without going through its internal buffer, and io.Pipe
// delivers one Write to exactly the Read call(s) that drain it. So writing exactly
// MaxFrame bytes in a single Write reliably produces exactly one MaxFrame-sized
// Read(), letting the test choose precisely where inside those bytes the nonce
// begins. The pipe is deliberately never closed: the real parked shell does not
// close its stdout either, so a carry-over that misses the nonce here must hang
// waiting for more data, the same failure mode production would see -- not return
// an error, which is why the assertion that matters most is a completion bound.
func TestDrainUntilNonceHandlesANonceSplitAcrossAReadBoundary(t *testing.T) {
	nonce, err := newNonce()
	if err != nil {
		t.Fatalf("newNonce: %v", err)
	}
	const exitCode = 7

	// Split partway through the nonce itself, computed from len(nonce) and MaxFrame
	// rather than a hard-coded offset: `before` bytes of the nonce land in the read
	// ending exactly at the MaxFrame boundary, and the remaining len(nonce)-before
	// land in the next one. If the nonce format ever changes length, this still
	// splits inside it instead of silently degrading into a same-read match.
	before := len(nonce) / 2
	prefixLen := MaxFrame - before
	prefix := strings.Repeat("a", prefixLen)
	sentinelTail := fmt.Sprintf("%s %d\n", nonce, exitCode)

	pr, pw := io.Pipe()
	r := bufio.NewReaderSize(pr, MaxFrame)
	t.Cleanup(func() { pw.Close() })

	go func() {
		// This first Write is exactly MaxFrame bytes (prefixLen + before), so
		// drainUntilNonce's first Read() returns exactly this much and no more --
		// see the function comment above for why that is guaranteed here.
		_, _ = pw.Write([]byte(prefix + sentinelTail[:before]))
		_, _ = pw.Write([]byte(sentinelTail[before:]))
		// Deliberately not closed here (see t.Cleanup above): a live, never-EOF
		// stream is what makes a missed nonce hang instead of erroring out.
	}()

	type result struct {
		code int32
		err  error
	}
	done := make(chan result, 1)
	var got []byte
	go func() {
		code, err := drainUntilNonce(r, nonce, func(b []byte) { got = append(got, b...) }, true)
		done <- result{code, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("drainUntilNonce error: %v", res.err)
		}
		if res.code != exitCode {
			t.Fatalf("exit code = %d, want %d: the sentinel was not parsed correctly across the split", res.code, exitCode)
		}
		if string(got) != prefix {
			t.Fatalf("forwarded %d bytes, want exactly the %d-byte prefix (no sentinel fragment leaked)", len(got), len(prefix))
		}
	case <-time.After(5 * time.Second):
		// The assertion that matters most: a broken carry-over does not return a
		// wrong answer here, it hangs forever waiting for a nonce that already flew
		// past it.
		t.Fatal("drainUntilNonce did not return within 5s: a broken carry-over hangs waiting for a nonce split across the read boundary")
	}
}

func TestAgentBoundsDrainAcrossMultipleReadCycles(t *testing.T) {
	const total = 160 * 1024 // 5x MaxFrame (32 KiB): forces multiple Read() cycles.
	const cap = 1000

	a := newTestAgent(t)
	start := time.Now()
	stdout, _, end, errMsg := drive(t, a, Request{
		Command:  fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'a'", total),
		CapBytes: cap,
	}, nil)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("drive took %s: a bounded drain must not buffer the whole line before capping", elapsed)
	}
	if errMsg != "" {
		t.Fatalf("agent error: %s", errMsg)
	}
	if len(stdout) != cap {
		t.Fatalf("forwarded %d stdout bytes, want exactly %d", len(stdout), cap)
	}
	if end.DroppedStdout != int64(total-cap) {
		t.Fatalf("DroppedStdout = %d, want exactly %d", end.DroppedStdout, total-cap)
	}
}

// TestAgentReportsATimeoutRatherThanHanging above only checks the FIRST command's
// error. Nothing drives a second command afterwards, so neither killShell actually
// running nor ServeConn's dead-check actually gating dispatch is pinned by it —
// either could be deleted and that test would still pass.
//
// This test drives a second, unrelated command on the SAME Agent after a timeout and
// asserts it fails fast with an explicit error mentioning the dead agent, rather than
// hanging (the orphaned drain goroutines racing a new command's, per fix round 1's
// Finding 3) or succeeding against a shell that no longer exists.
func TestAgentRejectsCommandsOnADeadAgentAfterATimeout(t *testing.T) {
	a := newTestAgent(t)
	_, _, _, errMsg := drive(t, a, Request{Command: "sleep 30", TimeoutS: 1, CapBytes: 1 << 20}, nil)
	if !strings.HasPrefix(errMsg, "timeout:") {
		t.Fatalf("first command's error = %q, want a timeout: prefix", errMsg)
	}

	start := time.Now()
	_, _, _, errMsg2 := drive(t, a, Request{Command: "echo hi", CapBytes: 1 << 20}, nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("second command took %s: a dead Agent must fail fast, not hang", elapsed)
	}
	if errMsg2 == "" {
		t.Fatal("second command on a dead agent succeeded; want an error")
	}
	if !strings.Contains(errMsg2, "dead") {
		t.Fatalf("second command's error = %q, want it to mention the agent is dead", errMsg2)
	}
}

// A review found that bash does not recompute $ or $PPID inside the "( ... )"
// subshell runOnParkedShell sources commands in — only $BASHPID differs there — so
// $PPID inside it is the parked shell's PARENT, which is the process running the
// Agent (cmd/guest-agent's main in production; this test binary here). A command
// using the common defensive idiom `trap 'kill $PPID' EXIT` therefore signals that
// process, not the parked shell. main.go installs signal.Ignore for exactly this
// reason; this test installs the same guard on itself (since here it is the test
// binary, not a guest-agent process, that sits where $PPID points) and then proves
// the parked shell AND the agent both survive the trap, with the command's own exit
// code still reported.
func TestAgentSurvivesTrapKillPPIDOnExit(t *testing.T) {
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	t.Cleanup(func() { signal.Reset(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP) })

	a := newTestAgent(t)
	pid := a.ShellPID()
	_, _, end, errMsg := drive(t, a, Request{
		Command: "trap 'kill $PPID' EXIT; echo hi; exit 3", CapBytes: 1 << 20,
	}, nil)
	if errMsg != "" {
		t.Fatalf("agent error: %s", errMsg)
	}
	if end.ExitCode != 3 {
		t.Fatalf("end.ExitCode = %d, want 3 (the command's own exit code, not a symptom of the agent dying)", end.ExitCode)
	}
	if a.ShellPID() != pid {
		t.Fatalf("ShellPID changed %d -> %d: the parked shell did not survive the trap", pid, a.ShellPID())
	}
	// Not just the shell — the agent itself must still be alive. Prove it with a
	// second, unrelated command over the same Agent.
	if _, _, end2, errMsg2 := drive(t, a, Request{Command: "echo still alive", CapBytes: 1 << 20}, nil); errMsg2 != "" || end2.ExitCode != 0 {
		t.Fatalf("second command after the trap: end=%+v err=%s", end2, errMsg2)
	}
}

func TestShouldSetClock(t *testing.T) {
	base := time.Unix(1757500000, 0)
	if shouldSetClock(base, base.Add(200*time.Millisecond)) {
		t.Error("a sub-second skew must not trigger a settimeofday on every Exec")
	}
	// The trap this exists for: the guest resumes from the SNAPSHOT moment, so the
	// skew is however long the golden snapshot has been in use — hours, not
	// milliseconds (spec §2.4).
	if !shouldSetClock(base, base.Add(-3*time.Hour)) {
		t.Error("a three-hour stale guest clock must be corrected")
	}
	if !shouldSetClock(base, base.Add(90*time.Second)) {
		t.Error("a 90s skew must be corrected")
	}
}
