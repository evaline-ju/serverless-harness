import { describe, expect, it } from 'vitest';
import type { CpError } from '../src/errors.js';
import {
  GithubOAuthProvider,
  adminSubjectsFromEnv,
  rolesFor,
  type FetchLike,
} from '../src/identity.js';

/** Records every request and replies from a scripted queue keyed by URL substring. */
function fakeFetch(script: Record<string, { status: number; body: unknown }[]>) {
  const seen: { url: string; method: string; headers: Record<string, string>; body?: string }[] =
    [];
  const fetch: FetchLike = async (url, init) => {
    seen.push({ url, ...init });
    const key = Object.keys(script).find((k) => url.includes(k));
    if (!key) throw new Error(`no scripted reply for ${url}`);
    const next = script[key]!.length > 1 ? script[key]!.shift()! : script[key]![0]!;
    return { status: next.status, text: async () => JSON.stringify(next.body) };
  };
  return { fetch, seen };
}

const provider = (fetch: FetchLike, adminSubjects: string[] = []) =>
  new GithubOAuthProvider({ clientId: 'Iv1.fakeclientid', adminSubjects, fetch }); // notsecret

const codeOf = async (fn: () => Promise<unknown>): Promise<string> => {
  try {
    await fn();
  } catch (e) {
    return (e as CpError).code;
  }
  throw new Error('expected a throw');
};

describe('the constructor refuses a missing client id', () => {
  // main.ts already validates SH_GITHUB_CLIENT_ID, but this guard is the one that holds if a second
  // caller ever constructs the provider directly -- and '' must fail exactly as undefined does, since
  // control-plane.yaml ships the variable present-but-empty on purpose.
  it('throws naming the env var, for both unset and empty', () => {
    for (const clientId of [undefined, '']) {
      expect(
        () => new GithubOAuthProvider({ clientId: clientId as string }),
        String(clientId),
      ).toThrow(/SH_GITHUB_CLIENT_ID is required/);
    }
  });

  it('accepts a non-empty one', () => {
    expect(() => new GithubOAuthProvider({ clientId: 'Iv1.abc' })).not.toThrow();
  });
});

describe('startDeviceAuth', () => {
  it('returns what an operator has to type, and how fast to poll', () => {
    const { fetch } = fakeFetch({
      '/login/device/code': [
        {
          status: 200,
          body: {
            device_code: 'dc-1',
            user_code: 'WDJB-MJHT',
            verification_uri: 'https://github.com/login/device',
            expires_in: 900,
            interval: 5,
          },
        },
      ],
    });
    return expect(provider(fetch).startDeviceAuth()).resolves.toEqual({
      deviceCode: 'dc-1',
      userCode: 'WDJB-MJHT',
      verificationUri: 'https://github.com/login/device',
      expiresIn: 900,
      interval: 5,
    });
  });

  it('asks for JSON, because GitHub otherwise replies form-encoded', async () => {
    const { fetch, seen } = fakeFetch({
      '/login/device/code': [
        { status: 200, body: { device_code: 'd', user_code: 'u', verification_uri: 'v' } },
      ],
    });
    await provider(fetch).startDeviceAuth();
    expect(seen[0]!.headers.Accept).toBe('application/json');
    expect(seen[0]!.method).toBe('POST');
    expect(seen[0]!.body).toContain('client_id=Iv1.fakeclientid'); // notsecret
  });

  it('sends no client secret — the device flow treats the app as a public client', async () => {
    // Which is also why §5.1.1 is safe to run from a script a developer reads.
    const { fetch, seen } = fakeFetch({
      '/login/device/code': [
        { status: 200, body: { device_code: 'd', user_code: 'u', verification_uri: 'v' } },
      ],
    });
    await provider(fetch).startDeviceAuth();
    expect(seen[0]!.body).not.toContain('client_secret');
  });

  it('defaults a missing interval to 5 seconds rather than to 0', async () => {
    // interval: 0 would make a polling client hot-loop GitHub into a slow_down.
    const { fetch } = fakeFetch({
      '/login/device/code': [
        { status: 200, body: { device_code: 'd', user_code: 'u', verification_uri: 'v' } },
      ],
    });
    expect((await provider(fetch).startDeviceAuth()).interval).toBe(5);
  });

  it('surfaces a GitHub error as unauthorized, not as a 500', async () => {
    const { fetch } = fakeFetch({
      '/login/device/code': [{ status: 404, body: { error: 'Not Found' } }],
    });
    expect(await codeOf(() => provider(fetch).startDeviceAuth())).toBe('unauthorized');
  });

  it('reports a device-flow-disabled app as unauthorized with a message an operator can act on', async () => {
    // Device flow is OFF by default on a GitHub OAuth app (spec §5.1.1); this is the most likely
    // first-run failure and the message is the only thing that makes it diagnosable.
    const { fetch } = fakeFetch({
      '/login/device/code': [
        { status: 400, body: { error: 'device_flow_disabled', error_description: 'not enabled' } },
      ],
    });
    await expect(provider(fetch).startDeviceAuth()).rejects.toThrow(/device_flow_disabled/);
  });
});

describe('completeDeviceAuth', () => {
  const authorized = {
    '/login/oauth/access_token': [{ status: 200, body: { access_token: 'gho-fake' } }], // notsecret
    '/user': [{ status: 200, body: { id: 1234, login: 'alice', name: 'Alice Example' } }],
  };

  it('derives the subject from the NUMERIC id, never the login', async () => {
    // The login is mutable and reusable after account deletion, so a login-keyed subject would let a
    // new account inherit a departed user's sessions and credentials (spec §5.1).
    const { fetch } = fakeFetch(authorized);
    expect(await provider(fetch).completeDeviceAuth('dc-1')).toEqual({
      subject: 'github:1234',
      displayName: 'Alice Example',
      roles: [],
    });
  });

  it('falls back to the login for the display name only', async () => {
    const { fetch } = fakeFetch({
      ...authorized,
      '/user': [{ status: 200, body: { id: 1234, login: 'alice', name: null } }],
    });
    const p = await provider(fetch).completeDeviceAuth('dc-1');
    expect(p.displayName).toBe('alice');
    expect(p.subject).toBe('github:1234');
  });

  it('stamps admin roles from the configured subject list', async () => {
    const { fetch } = fakeFetch(authorized);
    expect((await provider(fetch, ['github:1234']).completeDeviceAuth('dc-1')).roles).toEqual([
      'admin',
    ]);
    const { fetch: f2 } = fakeFetch(authorized);
    expect((await provider(f2, ['github:9999']).completeDeviceAuth('dc-1')).roles).toEqual([]);
  });

  it('never returns the GitHub access token', async () => {
    // MU1 stores no GitHub token at all: OAuth authenticates API CALLS, the stored credential
    // authorizes EGRESS, and the two are decoupled (spec §5.5) -- so no refresh token is needed and
    // the opaque token is used once, for GET /user, then dropped.
    const { fetch } = fakeFetch(authorized);
    const p = await provider(fetch).completeDeviceAuth('dc-1');
    expect(JSON.stringify(p)).not.toContain('gho-fake'); // notsecret
  });

  it('sends the access token as a Bearer to GET /user and nowhere else', async () => {
    const { fetch, seen } = fakeFetch(authorized);
    await provider(fetch).completeDeviceAuth('dc-1');
    const userCall = seen.find((c) => c.url.includes('/user'))!;
    expect(userCall.headers.Authorization).toBe('Bearer gho-fake'); // notsecret
    expect(seen.filter((c) => c.headers.Authorization).length).toBe(1);
  });

  it('maps authorization_pending to the pollable code, not to an error', async () => {
    const { fetch } = fakeFetch({
      '/login/oauth/access_token': [{ status: 200, body: { error: 'authorization_pending' } }],
    });
    expect(await codeOf(() => provider(fetch).completeDeviceAuth('dc-1'))).toBe(
      'authorization_pending',
    );
  });

  it('maps slow_down to the same pollable code', async () => {
    // A client that treated slow_down as fatal would abort a login that is still perfectly live.
    const { fetch } = fakeFetch({
      '/login/oauth/access_token': [{ status: 200, body: { error: 'slow_down', interval: 10 } }],
    });
    expect(await codeOf(() => provider(fetch).completeDeviceAuth('dc-1'))).toBe(
      'authorization_pending',
    );
  });

  it('maps a terminal GitHub error to unauthorized', async () => {
    for (const error of ['access_denied', 'expired_token', 'incorrect_device_code']) {
      const { fetch } = fakeFetch({
        '/login/oauth/access_token': [{ status: 200, body: { error } }],
      });
      expect(await codeOf(() => provider(fetch).completeDeviceAuth('dc-1')), error).toBe(
        'unauthorized',
      );
    }
  });

  it('rejects a /user reply with no numeric id rather than inventing a subject', async () => {
    const { fetch } = fakeFetch({
      ...authorized,
      '/user': [{ status: 200, body: { login: 'alice' } }],
    });
    expect(await codeOf(() => provider(fetch).completeDeviceAuth('dc-1'))).toBe('unauthorized');
  });

  it('rejects a non-JSON reply rather than throwing a raw SyntaxError', async () => {
    const fetch: FetchLike = async () => ({ status: 200, text: async () => '<html>502</html>' });
    expect(await codeOf(() => provider(fetch).completeDeviceAuth('dc-1'))).toBe('unauthorized');
  });

  it('rejects a JSON null reply rather than attempting to read its id', async () => {
    // typeof null === 'object', so the guard must check `parsed === null` explicitly.
    // This test pins that the `|| parsed === null` clause works (spec §5.5).
    const { fetch } = fakeFetch({
      '/login/oauth/access_token': [{ status: 200, body: { access_token: 'gho-fake' } }], // notsecret
      '/user': [{ status: 200, body: null }],
    });
    expect(await codeOf(() => provider(fetch).completeDeviceAuth('dc-1'))).toBe('unauthorized');
  });
});

describe('admin subjects', () => {
  it('parses a comma-separated list, ignoring blanks and whitespace', () => {
    expect(adminSubjectsFromEnv(' github:1 , github:2 ,, ')).toEqual(['github:1', 'github:2']);
  });

  it('is empty by default, so no deployment has an admin unless one is named', () => {
    expect(adminSubjectsFromEnv(undefined)).toEqual([]);
    expect(adminSubjectsFromEnv('')).toEqual([]);
    expect(rolesFor('github:1', [])).toEqual([]);
  });

  it('grants admin only on an exact subject match', () => {
    expect(rolesFor('github:1', ['github:1'])).toEqual(['admin']);
    expect(rolesFor('github:12', ['github:1'])).toEqual([]);
  });
});
