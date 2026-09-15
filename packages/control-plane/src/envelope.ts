import { createCipheriv, createDecipheriv, randomBytes } from 'node:crypto';

/**
 * Envelope encryption ON TOP of the Kubernetes Secret (spec §6.5). A Secret is base64, not
 * encryption, and namespace `get secrets` reads it -- so a namespace-wide secret read must yield
 * ciphertext. The KEK lives in a SEPARATE Secret mounted only into the control plane, making "read
 * the credential store" and "read the key that opens it" two distinct RBAC subjects.
 *
 * AES-256-GCM, and the AAD is `subject|name`. The AAD is free and buys a real property: an attacker
 * who can WRITE Secrets still cannot relabel Alice's ciphertext into Bob's row and spend her key,
 * because decryption then fails. Unambiguous because a credential name is restricted to
 * [a-z0-9][a-z0-9-]* (credential-store.ts), so no `|` can appear in either half.
 *
 * Wire form: `v1.<b64url iv>.<b64url tag>.<b64url ciphertext>`. Versioned so that moving to Vault
 * or External Secrets behind CredentialStore (spec §6.6) is distinguishable at rest rather than a
 * guess about which bytes are which.
 *
 * THE KEK IS A RING, for the same reason `parseKeyset` takes a list: without a dual-read window there
 * is no rotation, only a cutover that breaks every credential sealed before it. And because `/v1`
 * deliberately exposes no read-back path, nobody -- not even the control plane's own admin surface --
 * can export and re-seal, so a single-key cutover's only recovery is every user re-entering every
 * credential by hand. `seal` uses the FIRST key and `open` tries each, so rotation is: prepend the new
 * KEK, let writes re-seal forward, then drop the old one.
 *
 * The ring lives in the config rather than the wire form on purpose. A key id in the sealed value
 * would let `open` name the key it wanted instead of trying each, but it is also a format change, and
 * a format change is exactly what becomes a migration -- one needing the old KEK, which the no-read-
 * back rule forbids -- the moment the first credential is sealed. Try-each costs a few failed GCM
 * verifications on a ring of two and buys the rotation without one.
 *
 * What the key id WOULD also have bought is observability, so `open` returns the ring index instead:
 * it never leaves the process and changes no bytes at rest. Dropping the retired key is the one step
 * of the rotation an operator has to decide, and without the index the only signal they had decided
 * wrong was the outage it caused (`K8sSecretStore.get`, spec §6.5).
 */
export const KEK_BYTES = 32; // AES-256
const IV_BYTES = 12; // GCM standard nonce
const VERSION = 'v1';

/**
 * Parse `SH_CREDENTIAL_KEK`: one base64 32-byte key, or a comma-separated ring, NEWEST FIRST.
 *
 * Rejects an all-empty value rather than returning an empty ring: `seal` would then reach for
 * `keks[0]` and encrypt under `undefined`. A bad entry names its POSITION, because an operator adding
 * a second KEK mid-rotation is precisely who needs to know which of the two is wrong.
 */
export function keksFromBase64(raw: string | undefined): Buffer[] {
  const entries = (raw ?? '')
    .split(',')
    .map((e) => e.trim())
    .filter((e) => e.length > 0);
  if (entries.length === 0) {
    throw new Error('SH_CREDENTIAL_KEK is not set; the control plane cannot store credentials');
  }
  return entries.map((entry, i) => {
    const kek = Buffer.from(entry, 'base64');
    if (kek.length !== KEK_BYTES) {
      throw new Error(
        `SH_CREDENTIAL_KEK entry ${i + 1} must decode to exactly ${KEK_BYTES} bytes ` +
          `(got ${kek.length})`,
      );
    }
    return kek;
  });
}

export function credentialAad(subject: string, name: string): Buffer {
  return Buffer.from(`${subject}|${name}`);
}

/** Seals under `keks[0]`, so a write during a rotation always moves the ciphertext FORWARD. */
export function seal(keks: Buffer[], subject: string, name: string, plaintext: string): string {
  // Explicit, rather than letting an empty ring reach createCipheriv as `undefined` and surface as a
  // node crypto error that names neither the ring nor the credential.
  const primary = keks[0];
  if (!primary) throw new Error('cannot seal a credential: no KEK is configured');
  const iv = randomBytes(IV_BYTES);
  const cipher = createCipheriv('aes-256-gcm', primary, iv);
  cipher.setAAD(credentialAad(subject, name));
  const ct = Buffer.concat([cipher.update(plaintext, 'utf8'), cipher.final()]);
  return [
    VERSION,
    iv.toString('base64url'),
    cipher.getAuthTag().toString('base64url'),
    ct.toString('base64url'),
  ].join('.');
}

/**
 * What `open` recovered, and WHICH ring key recovered it.
 *
 * `keyIndex > 0` means "sealed under a key that is no longer primary", i.e. this credential has not
 * been re-sealed since the rotation began. It is the only in-process fact that distinguishes a
 * rotation still in flight from one that is complete, and it is for an operator, never for a caller
 * -- see `K8sSecretStore.get`, which is the sole place it is consumed.
 */
export interface Opened {
  plaintext: string;
  keyIndex: number;
}

/**
 * Tries every key in the ring, so ciphertext sealed before a rotation still reads.
 *
 * Version and structure are checked ONCE, before any key: those are properties of the sealed value,
 * not of a key, and re-reporting them per attempt would say nothing extra.
 *
 * The returned `keyIndex` is reported ONLY on success. A failure still yields one opaque message
 * carrying no index and no ring size, so trying N keys cannot become N distinguishable outcomes.
 */
export function open(keks: Buffer[], subject: string, name: string, sealed: string): Opened {
  const parts = sealed.split('.');
  if (parts.length !== 4) throw new Error('sealed credential is malformed');
  const [version, ivB64, tagB64, ctB64] = parts as [string, string, string, string];
  if (version !== VERSION) throw new Error(`unsupported sealed credential version '${version}'`);
  for (const [keyIndex, kek] of keks.entries()) {
    try {
      const decipher = createDecipheriv('aes-256-gcm', kek, Buffer.from(ivB64, 'base64url'));
      decipher.setAAD(credentialAad(subject, name));
      decipher.setAuthTag(Buffer.from(tagB64, 'base64url'));
      const plaintext = Buffer.concat([
        decipher.update(Buffer.from(ctB64, 'base64url')),
        decipher.final(),
      ]).toString('utf8');
      return { plaintext, keyIndex };
    } catch {
      // Keep trying: on a ring of two, the second key is the retired one this value was sealed under.
      // The AAD is re-bound per attempt, so a relabelled ciphertext is refused by EVERY key rather
      // than only by the primary.
    }
  }
  // One opaque message once the whole ring has failed -- wrong KEK, wrong subject, wrong name,
  // tampered ciphertext, or malformed IV/tag in the sealed format. Distinguishing them would tell a
  // caller which part of the input it guessed right, and a ring must not turn one outcome into N.
  throw new Error(`failed to decrypt credential '${name}'`);
}
