import { randomBytes } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { KEK_BYTES, credentialAad, keksFromBase64, open, seal } from '../src/envelope.js';

const KEK = randomBytes(KEK_BYTES); // generated per run -- never a committed key
const OTHER = randomBytes(KEK_BYTES);

describe('seal/open', () => {
  it('round-trips a secret', () => {
    const sealed = seal([KEK], 'github:1234', 'my-anthropic', 'sk-live-abc');
    expect(open([KEK], 'github:1234', 'my-anthropic', sealed).plaintext).toBe('sk-live-abc');
  });

  it('never emits the plaintext in the sealed form', () => {
    // One assertion, not two: re-base64ing the already-base64 sealed string cannot reveal a plaintext
    // the first assertion did not, so the second added no coverage.
    const sealed = seal([KEK], 'github:1234', 'my-anthropic', 'sk-live-abc');
    expect(sealed).not.toContain('sk-live-abc');
  });

  it('is versioned, so a later Vault/ESO backend is distinguishable at rest', () => {
    expect(seal([KEK], 'github:1', 'a', 'x').startsWith('v1.')).toBe(true);
  });

  it('uses a fresh iv per seal, so two seals of one value differ', () => {
    const a = seal([KEK], 'github:1', 'a', 'same');
    const b = seal([KEK], 'github:1', 'a', 'same');
    expect(a).not.toBe(b);
    expect(open([KEK], 'github:1', 'a', b).plaintext).toBe('same');
  });

  it('round-trips a long value and one with non-ASCII bytes', () => {
    const long = 'x'.repeat(8192);
    expect(open([KEK], 'github:1', 'a', seal([KEK], 'github:1', 'a', long)).plaintext).toBe(long);
    const utf8 = 'ключ-🔐-token';
    expect(open([KEK], 'github:1', 'a', seal([KEK], 'github:1', 'a', utf8)).plaintext).toBe(utf8);
  });
});

describe('the AAD buys a real property, not tidiness', () => {
  it('fails to open Alice ciphertext relabelled as Bob', () => {
    // An attacker who can WRITE Secrets still cannot move Alice's row into Bob's and spend her key
    // (spec §6.5). This is the whole reason the AAD exists.
    const sealed = seal([KEK], 'github:alice', 'my-anthropic', 'sk-alice');
    expect(() => open([KEK], 'github:bob', 'my-anthropic', sealed)).toThrow(/decrypt/i);
  });

  it('fails to open one credential name as another', () => {
    const sealed = seal([KEK], 'github:alice', 'my-anthropic', 'sk-alice');
    expect(() => open([KEK], 'github:alice', 'github-work', sealed)).toThrow(/decrypt/i);
  });

  it('binds subject and name in one unambiguous string', () => {
    expect(credentialAad('github:1234', 'my-anthropic').toString()).toBe(
      'github:1234|my-anthropic',
    );
  });
});

describe('failure modes', () => {
  it('fails with the wrong KEK', () => {
    const sealed = seal([KEK], 'github:1', 'a', 'x');
    expect(() => open([OTHER], 'github:1', 'a', sealed)).toThrow(/decrypt/i);
  });

  it('fails on a flipped ciphertext bit (GCM authenticates, it does not just encrypt)', () => {
    const sealed = seal([KEK], 'github:1', 'a', 'hello');
    const parts = sealed.split('.');
    const ct = Buffer.from(parts[3]!, 'base64url');
    ct[0] = ct[0]! ^ 0x01;
    parts[3] = ct.toString('base64url');
    expect(() => open([KEK], 'github:1', 'a', parts.join('.'))).toThrow(/decrypt/i);
  });

  it('rejects an unknown version prefix', () => {
    expect(() => open([KEK], 'github:1', 'a', 'v9.aa.bb.cc')).toThrow(/version/i);
  });

  it('rejects a structurally broken sealed value', () => {
    for (const broken of ['', 'v1', 'v1.a', 'v1.a.b', 'v1.a.b.c.d']) {
      expect(() => open([KEK], 'github:1', 'a', broken), broken).toThrow();
    }
    // 4 segments with wrong content lengths still fail with opaque error
    // IV too short (needs 12 bytes)
    expect(() =>
      open(
        [KEK],
        'github:1',
        'a',
        `v1.${randomBytes(11).toString('base64url')}.${randomBytes(16).toString('base64url')}.c`,
      ),
    ).toThrow(/decrypt/i);
    // IV too long (needs 12 bytes)
    expect(() =>
      open(
        [KEK],
        'github:1',
        'a',
        `v1.${randomBytes(13).toString('base64url')}.${randomBytes(16).toString('base64url')}.c`,
      ),
    ).toThrow(/decrypt/i);
    // Tag too short (needs 16 bytes)
    expect(() =>
      open(
        [KEK],
        'github:1',
        'a',
        `v1.${randomBytes(12).toString('base64url')}.${randomBytes(15).toString('base64url')}.c`,
      ),
    ).toThrow(/decrypt/i);
    // Tag too long (needs 16 bytes)
    expect(() =>
      open(
        [KEK],
        'github:1',
        'a',
        `v1.${randomBytes(12).toString('base64url')}.${randomBytes(17).toString('base64url')}.c`,
      ),
    ).toThrow(/decrypt/i);
  });

  it('rejects a KEK of the wrong length rather than padding it', () => {
    expect(() => keksFromBase64(randomBytes(16).toString('base64'))).toThrow(/32 bytes/);
    expect(() => keksFromBase64(undefined)).toThrow(/SH_CREDENTIAL_KEK/);
    expect(() => keksFromBase64('not-base64!!!')).toThrow(/32 bytes/);
  });

  it('accepts a correctly sized base64 KEK', () => {
    const raw = randomBytes(KEK_BYTES);
    expect(keksFromBase64(raw.toString('base64'))[0]!.equals(raw)).toBe(true);
  });
});

describe('KEK rotation', () => {
  // Before this, SH_CREDENTIAL_KEK was a single value: changing it made EVERY stored credential
  // undecryptable at once, with no dual-read window. And because /v1 deliberately has no read-back
  // path, nobody -- not even the control plane's own admin surface -- could export and re-seal, so
  // recovery was every user re-entering every credential by hand. Compare the token keyset, which
  // takes a LIST precisely so rotation needs no flag day (token.ts).

  it('opens ciphertext sealed under a RETIRED key once the new one is primary', () => {
    // The property that makes rotation possible at all: add the new KEK ahead of the old, and
    // everything sealed before the change still reads.
    const oldK = randomBytes(KEK_BYTES);
    const newK = randomBytes(KEK_BYTES);
    const sealedBefore = seal([oldK], 'github:1', 'a', 'sk-old');
    expect(open([newK, oldK], 'github:1', 'a', sealedBefore).plaintext).toBe('sk-old');
  });

  it('seals under the FIRST key, so a rotation re-seals forward and never backward', () => {
    // "Re-seal lazily on next write" only works if a write picks the new key. Proven by showing the
    // fresh ciphertext no longer opens under the retired key alone.
    const oldK = randomBytes(KEK_BYTES);
    const newK = randomBytes(KEK_BYTES);
    const sealedAfter = seal([newK, oldK], 'github:1', 'a', 'sk-new');
    expect(open([newK], 'github:1', 'a', sealedAfter).plaintext).toBe('sk-new');
    expect(() => open([oldK], 'github:1', 'a', sealedAfter)).toThrow(/decrypt/i);
  });

  it('completes the cycle: once re-sealed, the old key can be dropped', () => {
    const oldK = randomBytes(KEK_BYTES);
    const newK = randomBytes(KEK_BYTES);
    const resealed = seal([newK, oldK], 'github:1', 'a', 'sk-x');
    expect(open([newK], 'github:1', 'a', resealed).plaintext).toBe('sk-x');
  });

  it('parses a comma-separated ring, newest first, trimming whitespace', () => {
    const a = randomBytes(KEK_BYTES);
    const b = randomBytes(KEK_BYTES);
    const ring = keksFromBase64(` ${a.toString('base64')} , ${b.toString('base64')} `);
    expect(ring).toHaveLength(2);
    expect(ring[0]!.equals(a)).toBe(true);
    expect(ring[1]!.equals(b)).toBe(true);
  });

  it('names the offending POSITION when one entry of a ring is bad', () => {
    // A ring makes "which one is wrong" a real question, and an operator adding a second KEK is
    // exactly when they need the answer.
    const good = randomBytes(KEK_BYTES).toString('base64');
    expect(() => keksFromBase64(`${good},${randomBytes(16).toString('base64')}`)).toThrow(
      /entry 2/,
    );
    expect(() => keksFromBase64(`${randomBytes(31).toString('base64')},${good}`)).toThrow(
      /entry 1/,
    );
  });

  it('refuses an all-empty value rather than yielding an empty ring', () => {
    // An empty ring would make seal() reach for keks[0] and encrypt under undefined.
    for (const raw of ['', ' ', ',', ' , ']) {
      expect(() => keksFromBase64(raw), JSON.stringify(raw)).toThrow(/SH_CREDENTIAL_KEK/);
    }
  });

  it('refuses to seal with an empty ring instead of throwing deep inside createCipheriv', () => {
    expect(() => seal([], 'github:1', 'a', 'x')).toThrow(/no KEK/i);
  });

  it('keeps ONE opaque failure after trying every key, so a ring is no oracle', () => {
    // open()'s single message is deliberate (a caller must not learn which part of its input it got
    // right). Trying N keys must not turn into N distinguishable outcomes, and "wrong ring" must look
    // exactly like "tampered ciphertext".
    const sealed = seal([randomBytes(KEK_BYTES)], 'github:1', 'a', 'x');
    const wrongRing = [randomBytes(KEK_BYTES), randomBytes(KEK_BYTES), randomBytes(KEK_BYTES)];
    expect(() => open(wrongRing, 'github:1', 'a', sealed)).toThrow(
      /^failed to decrypt credential 'a'$/,
    );
    // Identical to the single-wrong-key message, and to a tampered one.
    expect(() => open([wrongRing[0]!], 'github:1', 'a', sealed)).toThrow(
      /^failed to decrypt credential 'a'$/,
    );
  });

  it('still enforces the AAD against EVERY key in the ring, not just the primary', () => {
    // The relabelling defence must not weaken as the ring grows: a retired key is still a key that
    // must refuse Alice's ciphertext under Bob's name.
    const oldK = randomBytes(KEK_BYTES);
    const newK = randomBytes(KEK_BYTES);
    const sealed = seal([oldK], 'github:alice', 'my-anthropic', 'sk-alice');
    expect(() => open([newK, oldK], 'github:bob', 'my-anthropic', sealed)).toThrow(/decrypt/i);
    expect(() => open([newK, oldK], 'github:alice', 'other-name', sealed)).toThrow(/decrypt/i);
  });

  it('reports WHICH ring key opened the value, so completing a rotation is observable', () => {
    // Step (c) of the rotation -- "drop the retired key" -- is the one step an operator has to
    // decide, and before this nothing in the system could tell them when it was safe. `open`
    // discarded the index it had just computed, so the only signal a key was dropped too early was
    // the failure it caused. A three-key ring on purpose: a boolean "was it the primary" passes a
    // two-key test and still cannot say which retired key is still in use.
    const keys = [randomBytes(KEK_BYTES), randomBytes(KEK_BYTES), randomBytes(KEK_BYTES)] as const;
    for (const [i, kek] of keys.entries()) {
      const sealed = seal([kek], 'github:1', 'a', `sk-${i}`);
      expect(open([...keys], 'github:1', 'a', sealed)).toEqual({
        plaintext: `sk-${i}`,
        keyIndex: i,
      });
    }
  });

  it('rejects an unknown version before trying any key', () => {
    const ring = [randomBytes(KEK_BYTES), randomBytes(KEK_BYTES)];
    expect(() => open(ring, 'github:1', 'a', 'v9.aa.bb.cc')).toThrow(/version/i);
  });
});
