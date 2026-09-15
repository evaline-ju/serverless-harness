import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { metricsBody } from '../src/admin.js';
import { harness } from './helpers/fake-worker.js';

/**
 * `ENV_ALLOWLIST` is the control that keeps an unauthenticated, loopback-bound endpoint from echoing
 * secrets, and its safety was by-convention only: nothing stopped a future addition of an
 * API-key-bearing variable from silently starting to appear on `/metrics`.
 *
 * The endpoint exists to prove which configuration produced a run (§5.3 pin 1), so the allowlist will
 * keep growing — every addition is a chance to add one whose value is a credential. These tests fail
 * on the SHAPE of the name rather than on a list of known-bad names, so they catch the variable nobody
 * thought of.
 */
const src = readFileSync(fileURLToPath(new URL('../src/admin.ts', import.meta.url)), 'utf8');

/** Names whose VALUE is, by convention in this repo, a credential. */
const SECRET_SHAPED = /(TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|_KEY|APIKEY)$/i;

function allowlistNames(): string[] {
  // Read from source rather than importing: ENV_ALLOWLIST is deliberately not exported, and making it
  // public just to test it would widen the surface this test exists to protect.
  const block = /const ENV_ALLOWLIST = \[([\s\S]*?)\] as const;/.exec(src);
  expect(block, 'ENV_ALLOWLIST literal not found in admin.ts').not.toBeNull();
  return [...block![1].matchAll(/'([^']+)'/g)].map((m) => m[1]!);
}

describe('the /metrics env allowlist stays secret-free', () => {
  it('finds the allowlist and it is non-empty', () => {
    // Guards the regex above: if the literal is ever reformatted so this stops matching, the two
    // assertions below would pass vacuously against an empty list.
    expect(allowlistNames().length).toBeGreaterThan(0);
  });

  it('contains no credential-shaped names', () => {
    const offenders = allowlistNames().filter((n) => SECRET_SHAPED.test(n));
    expect(
      offenders,
      `ENV_ALLOWLIST must not carry credential-shaped names -- /metrics is unauthenticated and ` +
        `loopback-bound, so anything here is readable by every local process: ${offenders.join(', ')}`,
    ).toEqual([]);
  });

  it('does not echo a credential-shaped variable even when one is set', () => {
    // The behavioural half. The name-shape check above could be satisfied by an allowlist that is
    // clean today; this asserts the endpoint filters rather than passing the environment through.
    const h = harness({}, 1);
    for (const w of h.forked) w.ready();
    const body = metricsBody(h.pool, {
      PORT: '8080',
      ANTHROPIC_API_KEY: 'sk-must-not-appear',
      SH_RELAY_TOKEN: 'must-not-appear',
      KAGENTI_SANDBOX_SECRET: 'must-not-appear',
    } as NodeJS.ProcessEnv);

    expect(body.env.PORT).toBe('8080');
    expect(JSON.stringify(body)).not.toContain('must-not-appear');
    for (const name of Object.keys(body.env)) {
      expect(SECRET_SHAPED.test(name), `${name} reached /metrics`).toBe(false);
    }
  });

  it('proves the guard can fail: the pattern matches the names it is meant to catch', () => {
    // An absence-assertion that has never been shown capable of failing is asserting nothing.
    for (const bad of [
      'ANTHROPIC_API_KEY',
      'SH_RELAY_TOKEN',
      'REDIS_PASSWORD',
      'SOME_SECRET',
      'MY_CREDENTIAL',
    ]) {
      expect(SECRET_SHAPED.test(bad), bad).toBe(true);
    }
    // And that it does not fire on the names legitimately present today.
    for (const ok of ['PORT', 'SH_WORKERS', 'SH_TURNS_PER_WORKER', 'ANTHROPIC_BASE_URL']) {
      expect(SECRET_SHAPED.test(ok), ok).toBe(false);
    }
  });
});
