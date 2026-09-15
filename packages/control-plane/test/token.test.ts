import { createHash, generateKeyPairSync, sign as cryptoSign } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { CpError } from '../src/errors.js';
import {
  DEFAULT_ISSUER,
  TOKEN_AUDIENCE,
  keyIdFor,
  makeSigner,
  parseKeyset,
  publicKeyFromBase64,
  publicKeyToBase64,
  verifyToken,
} from '../src/token.js';

// Keys are GENERATED per test run, never embedded. A committed Ed25519 private key would be a
// gitleaks finding on the blob (which no later `# notsecret` comment can retract) and `.gitignore`
// already refuses `*.pem` / `*.key`, so a fixture file is not an option either.
function keypair() {
  const { privateKey, publicKey } = generateKeyPairSync('ed25519');
  return {
    publicKey,
    privatePem: privateKey.export({ format: 'pem', type: 'pkcs8' }).toString(),
    publicPem: publicKey.export({ format: 'pem', type: 'spki' }).toString(),
  };
}

const NOW = 1_757_000_000; // fixed epoch seconds, so expiry assertions are not clock-dependent

function mintApi(
  privatePem: string,
  over: Partial<Parameters<ReturnType<typeof makeSigner>['mint']>[0]> = {},
) {
  return makeSigner(privatePem).mint({
    sub: 'github:1234',
    tenant: 'github:1234',
    roles: [],
    scope: ['api'],
    ttlSeconds: 3600,
    now: NOW,
    ...over,
  });
}

describe('key encoding and kid derivation', () => {
  it('derives the kid from the key itself, so the two cannot drift', () => {
    const { publicKey } = keypair();
    const spki = publicKey.export({ format: 'der', type: 'spki' }) as Buffer;
    expect(keyIdFor(publicKey)).toBe(createHash('sha256').update(spki).digest('hex').slice(0, 16));
  });

  it('round-trips a public key through the base64 DER SPKI wire form', () => {
    const { publicKey } = keypair();
    const back = publicKeyFromBase64(publicKeyToBase64(publicKey));
    expect(back.type).toBe('public');
    expect(keyIdFor(back)).toBe(keyIdFor(publicKey));
  });

  it('parses a comma-separated multi-key keyset, which is what makes rotation flag-day-free', () => {
    const a = keypair();
    const b = keypair();
    const raw = `${keyIdFor(a.publicKey)}:${publicKeyToBase64(a.publicKey)},${keyIdFor(b.publicKey)}:${publicKeyToBase64(b.publicKey)}`;
    const keys = parseKeyset(raw);
    expect([...keys.keys()].sort()).toEqual([keyIdFor(a.publicKey), keyIdFor(b.publicKey)].sort());
  });

  it('tolerates surrounding whitespace and a trailing comma', () => {
    const a = keypair();
    const keys = parseKeyset(` ${keyIdFor(a.publicKey)} : ${publicKeyToBase64(a.publicKey)} , `);
    expect(keys.size).toBe(1);
  });

  it('returns an empty keyset for unset, so a caller decides what that means', () => {
    expect(parseKeyset(undefined).size).toBe(0);
    expect(parseKeyset('').size).toBe(0);
  });

  it('throws on a malformed entry rather than silently dropping it', () => {
    // A dropped key is a deployment that rejects every token minted with it, discovered at runtime.
    expect(() => parseKeyset('no-colon-here')).toThrow(/SH_SESSION_TOKEN_PUBLIC_KEYS/);
    expect(() => parseKeyset('kid:not-base64-der')).toThrow(/SH_SESSION_TOKEN_PUBLIC_KEYS/);
  });

  it('rejects a keyset entry whose kid does not match its key', () => {
    const a = keypair();
    expect(() => parseKeyset(`deadbeefdeadbeef:${publicKeyToBase64(a.publicKey)}`)).toThrow(/kid/);
  });
});

describe('the verifier cannot mint', () => {
  it('refuses to build a signer from a public key', () => {
    // The whole argument for Ed25519 over HMAC (spec §5.2) is that the data plane, which holds only
    // the public half, cannot forge a token. Asserted on the API, not left to callers.
    const { publicPem } = keypair();
    expect(() => makeSigner(publicPem)).toThrow(/private/i);
  });

  it('hands out only public KeyObjects from a keyset', () => {
    const a = keypair();
    const keys = parseKeyset(`${keyIdFor(a.publicKey)}:${publicKeyToBase64(a.publicKey)}`);
    const only = [...keys.values()][0]!;
    expect(only.type).toBe('public');
    expect(() => cryptoSign(null, Buffer.from('x'), only)).toThrow();
  });
});

describe('mint and verify', () => {
  it('round-trips an api token', () => {
    const { privatePem, publicKey } = keypair();
    const signer = makeSigner(privatePem);
    const token = mintApi(privatePem);
    const keys = new Map([[signer.kid, publicKey]]);
    const claims = verifyToken(token, keys, { now: NOW, requiredScope: 'api' });
    expect(claims.sub).toBe('github:1234');
    expect(claims.aud).toBe(TOKEN_AUDIENCE);
    expect(claims.exp).toBe(NOW + 3600);
    expect(claims.sid).toBeUndefined();
    expect(claims.jti).toMatch(/^[0-9a-f-]{36}$/);
  });

  it('round-trips a session token carrying sid', () => {
    const { privatePem, publicKey } = keypair();
    const signer = makeSigner(privatePem);
    const token = signer.mint({
      sub: 'github:1234',
      tenant: 'github:1234',
      roles: [],
      scope: ['turn:write'],
      ttlSeconds: 300,
      sid: 'sess-1',
      now: NOW,
    });
    const claims = verifyToken(token, new Map([[signer.kid, publicKey]]), {
      now: NOW,
      requiredScope: 'turn:write',
    });
    expect(claims.sid).toBe('sess-1');
    expect(claims.exp).toBe(NOW + 300);
  });

  it('puts the kid in the JOSE header so a verifier can pick the key without trying all of them', () => {
    const { privatePem } = keypair();
    const header = JSON.parse(
      Buffer.from(mintApi(privatePem).split('.')[0]!, 'base64url').toString(),
    );
    expect(header).toEqual({ alg: 'EdDSA', typ: 'JWT', kid: makeSigner(privatePem).kid });
  });

  it('rejects an expired token', () => {
    const { privatePem, publicKey } = keypair();
    const signer = makeSigner(privatePem);
    const token = mintApi(privatePem, { ttlSeconds: 300 });
    const err = (() => {
      try {
        verifyToken(token, new Map([[signer.kid, publicKey]]), { now: NOW + 301 });
      } catch (e) {
        return e as CpError;
      }
    })();
    expect(err).toBeInstanceOf(CpError);
    expect(err!.code).toBe('token_expired');
  });

  it('accepts a token exactly at its expiry second and rejects the next', () => {
    const { privatePem, publicKey } = keypair();
    const signer = makeSigner(privatePem);
    const keys = new Map([[signer.kid, publicKey]]);
    const token = mintApi(privatePem, { ttlSeconds: 300 });
    expect(() => verifyToken(token, keys, { now: NOW + 300 })).not.toThrow();
    expect(() => verifyToken(token, keys, { now: NOW + 301 })).toThrow();
  });

  it('rejects a tampered payload', () => {
    const { privatePem, publicKey } = keypair();
    const signer = makeSigner(privatePem);
    const [h, p, s] = mintApi(privatePem).split('.');
    const claims = JSON.parse(Buffer.from(p!, 'base64url').toString());
    claims.sub = 'github:9999'; // become someone else
    const forged = `${h}.${Buffer.from(JSON.stringify(claims)).toString('base64url')}.${s}`;
    expect(() => verifyToken(forged, new Map([[signer.kid, publicKey]]))).toThrow(
      expect.objectContaining({ code: 'token_invalid' }),
    );
  });

  it('rejects an unknown kid without attempting the other keys', () => {
    const a = keypair();
    const b = keypair();
    const token = mintApi(a.privatePem);
    expect(() =>
      verifyToken(token, new Map([[keyIdFor(b.publicKey), b.publicKey]]), { now: NOW }),
    ).toThrow(expect.objectContaining({ code: 'token_invalid' }));
  });

  it('rejects the alg-none downgrade and any non-EdDSA alg', () => {
    const { privatePem, publicKey } = keypair();
    const keys = new Map([[keyIdFor(publicKey), publicKey]]);
    const header = { alg: 'none', typ: 'JWT', kid: keyIdFor(publicKey) };
    const payload = { sub: 'github:1', aud: TOKEN_AUDIENCE, exp: NOW + 60, scope: ['api'] };
    const b64 = (o: unknown) => Buffer.from(JSON.stringify(o)).toString('base64url');
    const h = b64(header);
    const p = b64(payload);
    const s = cryptoSign(null, Buffer.from(`${h}.${p}`), { key: privatePem }).toString('base64url');
    expect(() => verifyToken(`${h}.${p}.${s}`, keys, { now: NOW })).toThrow(
      expect.objectContaining({ code: 'token_invalid' }),
    );
  });

  it('rejects a wrong audience', () => {
    const { privatePem, publicKey } = keypair();
    const signer = makeSigner(privatePem);
    // Mint with the right aud, then re-sign a different aud with the SAME key: the signature is
    // valid, so only the aud check can reject it.
    const claims = JSON.parse(
      Buffer.from(mintApi(privatePem).split('.')[1]!, 'base64url').toString(),
    );
    claims.aud = 'some-other-service';
    const h = Buffer.from(JSON.stringify({ alg: 'EdDSA', typ: 'JWT', kid: signer.kid })).toString(
      'base64url',
    );
    const p = Buffer.from(JSON.stringify(claims)).toString('base64url');
    const s = cryptoSign(null, Buffer.from(`${h}.${p}`), {
      key: privatePem,
    }).toString('base64url');
    expect(() =>
      verifyToken(`${h}.${p}.${s}`, new Map([[signer.kid, publicKey]]), { now: NOW }),
    ).toThrow(expect.objectContaining({ code: 'token_invalid' }));
  });

  it('rejects a token whose scope does not include the required one', () => {
    const { privatePem, publicKey } = keypair();
    const signer = makeSigner(privatePem);
    // An api token must not be able to drive a turn, and a session token must not be able to
    // rewrite credentials (plan gap #7).
    const api = mintApi(privatePem);
    expect(() =>
      verifyToken(api, new Map([[signer.kid, publicKey]]), {
        now: NOW,
        requiredScope: 'turn:write',
      }),
    ).toThrow(expect.objectContaining({ code: 'token_invalid' }));
  });

  it('rejects structurally broken input rather than throwing a raw TypeError', () => {
    const { publicKey } = keypair();
    const keys = new Map([[keyIdFor(publicKey), publicKey]]);
    for (const bad of ['', 'a', 'a.b', 'a.b.c.d', '..', 'not-base64url.$$$.%%%']) {
      expect(() => verifyToken(bad, keys), bad).toThrow(
        expect.objectContaining({ code: 'token_invalid' }),
      );
    }
  });

  it('rejects every token when the keyset is empty', () => {
    const { privatePem } = keypair();
    expect(() => verifyToken(mintApi(privatePem), new Map(), { now: NOW })).toThrow(
      expect.objectContaining({ code: 'token_invalid' }),
    );
  });

  it('rejects a wrong ISSUER, even validly signed by a key in the keyset', () => {
    // `iss` was minted -- MintInput lets a caller set it -- but never checked, so the claim was
    // decorative. It matters as soon as a deployment publishes more than one issuer's key, which is
    // exactly what the keyset LIST exists to allow (spec §5.2).
    const { privatePem, publicKey } = keypair();
    const signer = makeSigner(privatePem);
    const token = mintApi(privatePem, { iss: 'some-other-control-plane' });
    expect(() => verifyToken(token, new Map([[signer.kid, publicKey]]), { now: NOW })).toThrow(
      expect.objectContaining({ code: 'token_invalid' }),
    );
    // ...and the default issuer still round-trips, so the check is not simply refusing everything.
    expect(
      verifyToken(mintApi(privatePem), new Map([[signer.kid, publicKey]]), { now: NOW }).iss,
    ).toBe(DEFAULT_ISSUER);
  });

  it('rejects a token whose header or payload decodes to JSON null', () => {
    // `JSON.parse('null')` succeeds and yields null, so `typeof x === 'object'` is true for it and a
    // bare typeof check would wave it through into a property read on null. Mutation-verified in
    // Task 2's fix round but never pinned by a test until now.
    const { privatePem, publicKey } = keypair();
    const signer = makeSigner(privatePem);
    const keys = new Map([[signer.kid, publicKey]]);
    const b64 = (o: unknown) => Buffer.from(JSON.stringify(o)).toString('base64url');
    const goodHeader = b64({ alg: 'EdDSA', typ: 'JWT', kid: signer.kid });
    const goodPayload = mintApi(privatePem).split('.')[1]!;
    const resign = (h: string, p: string) => {
      const sig = cryptoSign(null, Buffer.from(`${h}.${p}`), { key: privatePem }).toString(
        'base64url',
      );
      return `${h}.${p}.${sig}`;
    };
    // A null HEADER cannot reach the key lookup, so it must be refused before it.
    expect(() => verifyToken(resign(b64(null), goodPayload), keys, { now: NOW })).toThrow(
      expect.objectContaining({ code: 'token_invalid' }),
    );
    // A null PAYLOAD is validly signed by a known key -- only the payload guard can reject it.
    expect(() => verifyToken(resign(goodHeader, b64(null)), keys, { now: NOW })).toThrow(
      expect.objectContaining({ code: 'token_invalid' }),
    );
  });
});

describe('a public key must be Ed25519, and is rejected at parse time if not', () => {
  // Not exploitable without the check -- a wrong curve simply fails cryptoVerify with alg: EdDSA -- but
  // the failure then arrives per request as "bad token signature" on a healthy-looking deployment.
  // parseKeyset runs at STARTUP on both tiers, so this turns a mislabelled key into a boot error.
  it('refuses an RSA public key from publicKeyFromBase64', () => {
    const { publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
    expect(() => publicKeyFromBase64(publicKeyToBase64(publicKey))).toThrow(/ed25519/);
  });

  it('refuses a P-256 public key too, not just RSA', () => {
    const { publicKey } = generateKeyPairSync('ec', { namedCurve: 'P-256' });
    expect(() => publicKeyFromBase64(publicKeyToBase64(publicKey))).toThrow(/ed25519/);
  });

  it('refuses a whole keyset entry whose key is the wrong curve', () => {
    const { publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
    // The kid is derived from the SPKI either way, so this entry is self-consistent -- the ONLY thing
    // that can reject it is the curve assertion.
    const entry = `${keyIdFor(publicKey)}:${publicKeyToBase64(publicKey)}`;
    expect(() => parseKeyset(entry)).toThrow(/Ed25519 SPKI/);
  });

  it('still accepts a real Ed25519 keyset', () => {
    const { publicKey } = keypair();
    const entry = `${keyIdFor(publicKey)}:${publicKeyToBase64(publicKey)}`;
    expect([...parseKeyset(entry).keys()]).toEqual([keyIdFor(publicKey)]);
  });
});
