import { describe, it, expect } from 'vitest';
import { cpus } from 'node:os';
import { defaultWorkers, readConfig } from '../src/config.js';

const base = { SH_TURNS_PER_WORKER: '8' } as NodeJS.ProcessEnv;
const env = (extra: Record<string, string> = {}) => ({ ...base, ...extra }) as NodeJS.ProcessEnv;

describe('readConfig', () => {
  it('fills the defaults spec §3.8 names', () => {
    const c = readConfig(env());
    expect(c.port).toBe(8080);
    expect(c.workers).toBe(defaultWorkers(cpus().length));
    expect(c.restartBackoffMs).toBe(250);
    expect(c.policy.name).toBe('leastInFlight');
  });

  it('reads every documented override', () => {
    const c = readConfig(
      env({
        PORT: '9090',
        SH_WORKERS: '3',
        SH_TURNS_PER_WORKER: '12',
        SH_ROUTING_POLICY: 'stickyBySession',
        SH_WORKER_RESTART_BACKOFF_MS: '500',
      }),
    );
    expect(c).toMatchObject({
      port: 9090,
      workers: 3,
      turnsPerWorker: 12,
      restartBackoffMs: 500,
    });
    expect(c.policy.name).toBe('stickyBySession');
  });

  it('refuses to boot without SH_TURNS_PER_WORKER, and says why there is no default', () => {
    // §3.8: every plausible default either hides the density this slice exists to find or
    // invites the thrash E8 is meant to locate. Failing to start is the honest behaviour.
    expect(() => readConfig({} as NodeJS.ProcessEnv)).toThrow(/SH_TURNS_PER_WORKER is required/);
    expect(() => readConfig({} as NodeJS.ProcessEnv)).toThrow(/no default/);
  });

  it('rejects a non-numeric or non-positive S', () => {
    expect(() => readConfig(env({ SH_TURNS_PER_WORKER: 'lots' }))).toThrow(
      /SH_TURNS_PER_WORKER='lots'/,
    );
    expect(() => readConfig(env({ SH_TURNS_PER_WORKER: '0' }))).toThrow(/SH_TURNS_PER_WORKER='0'/);
  });

  it('rejects a non-positive worker count', () => {
    expect(() => readConfig(env({ SH_WORKERS: '0' }))).toThrow(/SH_WORKERS='0'/);
  });

  it('never defaults W to 0, however many CPUs the host reports', () => {
    // `readInt` returned its `fallback` unchecked, and `SH_WORKERS`'s fallback is COMPUTED:
    // `cpus().length`, where `os.cpus()` is documented as possibly returning an empty array. With
    // `SH_WORKERS` unset on such a host the supervisor booted successfully, logged
    // `supervisor_listening ... workers: 0`, forked nothing, and then answered 429 to every request
    // for the life of the process -- `isSaturated([])` is `true` by design -- with no error
    // anywhere to say why. 0 is not a legal W, and the host is not the operator's mistake, so the
    // default is clamped rather than made a boot failure.
    expect(defaultWorkers(0)).toBe(1);
    expect(defaultWorkers(1)).toBe(1);
    expect(defaultWorkers(8)).toBe(8);
    // And the bounds now cover the fallback path too, so the next computed default that is out of
    // range is a boot failure naming itself rather than a silently wedged supervisor.
    expect(readConfig(env()).workers).toBeGreaterThanOrEqual(1);
  });

  it('allows a zero restart backoff but not a negative one', () => {
    expect(readConfig(env({ SH_WORKER_RESTART_BACKOFF_MS: '0' })).restartBackoffMs).toBe(0);
    expect(() => readConfig(env({ SH_WORKER_RESTART_BACKOFF_MS: '-1' }))).toThrow(
      /SH_WORKER_RESTART_BACKOFF_MS='-1'/,
    );
  });

  it('allows PORT=0 so tests can bind an ephemeral port', () => {
    expect(readConfig(env({ PORT: '0' })).port).toBe(0);
    expect(() => readConfig(env({ PORT: '70000' }))).toThrow(/PORT='70000'/);
  });

  it('propagates SH_ROUTING_POLICY validation instead of silently defaulting', () => {
    expect(() => readConfig(env({ SH_ROUTING_POLICY: 'random' }))).toThrow(
      /SH_ROUTING_POLICY='random'/,
    );
  });

  it('defaults the admin port beside the data port and allows 0 for tests', () => {
    expect(readConfig(env()).adminPort).toBe(8081);
    expect(readConfig(env({ SH_ADMIN_PORT: '0' })).adminPort).toBe(0);
    expect(() => readConfig(env({ SH_ADMIN_PORT: 'x' }))).toThrow(/SH_ADMIN_PORT/);
  });

  it('refuses an admin port equal to the data port', () => {
    // Two listeners on one port is an EADDRINUSE at boot in the best case and, if the data
    // listener wins the race, a supervisor that silently has no telemetry at all.
    expect(() => readConfig(env({ PORT: '9000', SH_ADMIN_PORT: '9000' }))).toThrow(/SH_ADMIN_PORT/);
    // 0 twice is not a collision: the kernel picks two different ephemeral ports.
    expect(readConfig(env({ PORT: '0', SH_ADMIN_PORT: '0' })).adminPort).toBe(0);
  });
});
