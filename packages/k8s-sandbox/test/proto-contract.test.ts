import { describe, it, expect } from 'vitest';
import { Hello, Exec, End } from '../src/gen/sandbox/v1/sandbox';
import { Chunk, Stream } from '../src/gen/sandbox/v1/sandbox.js';

describe('sandbox/v1 generated TypeScript stubs', () => {
  it('round-trips a Hello through binary encode/decode', () => {
    const bytes = Hello.encode({
      sandboxId: 'sbx-1',
      labels: { team: 'alpha' },
      capabilities: ['python3', 'kubectl'],
      image: 'img@sha256:abc',
      arch: 'amd64',
      capacityMax: 4,
      trust: 'trusted',
    }).finish();

    const back = Hello.decode(bytes);
    expect(back.sandboxId).toBe('sbx-1');
    expect(back.labels.team).toBe('alpha');
    expect(back.capabilities).toEqual(['python3', 'kubectl']);
    expect(back.capacityMax).toBe(4);
  });

  it('keeps req_id as a number and stdin as bytes', () => {
    const back = Exec.decode(
      Exec.encode({
        reqId: 7,
        command: 'echo hi',
        stdin: new Uint8Array([1, 2, 3]),
        timeoutS: 30,
        streaming: true,
        workspaceKey: '',
      }).finish(),
    );
    expect(back.reqId).toBe(7);
    expect(back.streaming).toBe(true);
    expect(Array.from(back.stdin)).toEqual([1, 2, 3]);
  });

  it('carries workspace_key and defaults it to the empty string', () => {
    const back = Exec.decode(
      Exec.encode({
        reqId: 1,
        command: 'cat /workspace/README.md',
        stdin: new Uint8Array(),
        timeoutS: 30,
        streaming: false,
        workspaceKey: 'leaf-abc123',
      }).finish(),
    );
    expect(back.workspaceKey).toBe('leaf-abc123');

    // Additive and backward-compatible (spec §3.4): a worker or harness that never
    // sets the field reads it as '', which is proto3's default and is exactly what
    // remote-worker's "today's behaviour" pin depends on.
    const old = Exec.decode(
      Exec.encode({
        reqId: 2,
        command: 'true',
        stdin: new Uint8Array(),
        timeoutS: 0,
        streaming: true,
        workspaceKey: '',
      }).finish(),
    );
    expect(old.workspaceKey).toBe('');
  });

  it('preserves a negative exit_code (sint32 zigzag)', () => {
    const back = End.decode(End.encode({ reqId: 7, exitCode: -9, truncated: false }).finish());
    expect(back.exitCode).toBe(-9);
  });

  // #189: truncated travels alongside the command's real exit code, not instead of
  // it. The worker reports what the command did; mapping truncation to a null status
  // is the transport's job (spec §8), so both values must survive the wire together.
  it('carries truncated alongside a real exit_code', () => {
    const back = End.decode(End.encode({ reqId: 7, exitCode: 0, truncated: true }).finish());
    expect(back.truncated).toBe(true);
    expect(back.exitCode).toBe(0);
  });

  // A worker that never drops output never sets the field, and proto3's default must
  // read as "not truncated" — the pre-#189 behaviour, which is correct for it.
  it('defaults truncated to false when a worker omits it', () => {
    const back = End.decode(End.encode({ reqId: 7, exitCode: 0, truncated: false }).finish());
    expect(back.truncated).toBe(false);
  });
});

describe('Chunk stream discriminator (ST3)', () => {
  it('exposes the Stream enum with stdout/stderr', () => {
    expect(Stream.STREAM_STDOUT).toBe(1);
    expect(Stream.STREAM_STDERR).toBe(2);
    expect(Stream.STREAM_UNSPECIFIED).toBe(0);
  });

  it('round-trips a Chunk carrying stderr', () => {
    const c = Chunk.decode(
      Chunk.encode({
        reqId: 7,
        data: new Uint8Array([1, 2]),
        stream: Stream.STREAM_STDERR,
      }).finish(),
    );
    expect(c.reqId).toBe(7);
    expect(c.stream).toBe(Stream.STREAM_STDERR);
    expect([...c.data]).toEqual([1, 2]);
  });
});
