import { createHash } from 'node:crypto';
import { open, seal, type Opened } from './envelope.js';
import { CpError } from './errors.js';
import {
  buildCreateSecretArgs,
  buildGetSecretArgs,
  buildPatchSecretArgs,
  defaultRunKubectl,
  isAlreadyExists,
  type RunKubectl,
} from './kubectl.js';
import type {
  Consumer,
  CredentialDescriptor,
  CredentialStore,
  StoredCredential,
} from './credential-store.js';

/**
 * Per-user Kubernetes Secrets in a DEDICATED namespace, under envelope encryption (spec §6.5).
 *
 * Redis cannot hold these: it is entirely ephemeral (spec §2.5 -- no PVC, no volume, no appendonly),
 * so a pod bounce would lose every user's key and re-entry after each restart would make the system
 * fragile in its most visible place. Credentials are the one thing that must outlive every session.
 *
 * The Secret NAME is derived from the subject (sha256, 16 hex chars), which is what makes every
 * access a `get` by exact name -- so the runtime Role can omit the `list` verb and a bug or an
 * injection cannot enumerate users. A hash also means object names disclose no logins.
 *
 * Writes are MERGE PATCHES, never `apply`: apply would replace the whole Secret and drop the
 * subject's other credentials, and read-modify-write would lose updates -- the exact failure mode
 * spec §6.6 rejects a single shared Secret over.
 *
 * Per-user Secrets are right at demo and team scale and a known anti-pattern at very large user
 * counts (one etcd object each, with watch and informer cost). The recorded direction is an external
 * manager -- Vault, or External Secrets -- behind the same CredentialStore interface (spec §6.6).
 */
export function subjectHash(subject: string): string {
  return createHash('sha256').update(subject).digest('hex').slice(0, 16);
}

export function secretNameFor(subject: string): string {
  return `sh-cred-${subjectHash(subject)}`;
}

const ANNOTATION_PREFIX = 'sh.io';
const FIELDS = ['kind', 'consumer', 'destination', 'binding', 'endpoint'] as const;
const annotationKey = (field: (typeof FIELDS)[number], name: string): string =>
  `${ANNOTATION_PREFIX}/${field}.${name}`;

interface SecretJson {
  metadata?: { annotations?: Record<string, string> };
  data?: Record<string, string>;
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === 'object' && v !== null && !Array.isArray(v);

/**
 * Parse `kubectl get -o json` output into a SecretJson, or refuse in a TYPED way.
 *
 * `JSON.parse(raw) as SecretJson` asserted a shape it had not checked, so malformed kubectl output
 * (a proxy's HTML error page, a truncated read) escaped as a raw SyntaxError and became a 500
 * `internal_error` -- a code that says "the control plane has a bug" about something that is
 * squarely "the credential store is not answering right now".
 */
function parseSecretJson(raw: string): SecretJson {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new CpError('credential_unavailable', 'the credential store returned a non-JSON body');
  }
  if (!isRecord(parsed)) {
    throw new CpError(
      'credential_unavailable',
      'the credential store returned an unexpected shape',
    );
  }
  // metadata/data are both optional in the wire object, so a wrong TYPE on either is the only thing
  // left to reject; every consumer below already treats absent as empty.
  const { metadata, data } = parsed as { metadata?: unknown; data?: unknown };
  if (metadata !== undefined && !isRecord(metadata)) {
    throw new CpError(
      'credential_unavailable',
      'the credential store returned unexpected metadata',
    );
  }
  if (data !== undefined && !isRecord(data)) {
    throw new CpError('credential_unavailable', 'the credential store returned unexpected data');
  }
  return parsed as SecretJson;
}

export class K8sSecretStore implements CredentialStore {
  private readonly namespace: string;
  /** The KEK ring, newest first: `seal` uses the first, `open` tries each (envelope.ts). */
  private readonly keks: Buffer[];
  private readonly run: RunKubectl;

  constructor(opts: { namespace: string; keks: Buffer[]; run?: RunKubectl }) {
    this.namespace = opts.namespace;
    this.keks = opts.keks;
    this.run = opts.run ?? defaultRunKubectl;
  }

  /**
   * Turn a kubectl / Kubernetes-API failure into `credential_unavailable` (503) rather than letting it
   * escape as a plain Error and become 500 `internal_error`.
   *
   * The taxonomy already has the right code and nothing on the control-plane side mapped to it: an
   * unreachable API server, an expired ServiceAccount token, a 403 from a narrowed Role are all "the
   * credential store is not answering", which is a 503 the caller can retry -- not a 500 that says the
   * control plane is broken. The exact mirror of OwnershipIndex.guard, and for the same reason: this
   * class is the only thing that knows a call was a credential-store call.
   *
   * A CpError from a nested call is already typed and passes through unwrapped. The message never
   * carries kubectl's stderr, which can hold a token or a resource name.
   */
  private async guard<T>(op: () => Promise<T>): Promise<T> {
    try {
      return await op();
    } catch (err) {
      if (err instanceof CpError) throw err;
      throw new CpError('credential_unavailable', 'the credential store is not answering');
    }
  }

  private async fetch(subject: string): Promise<SecretJson | null> {
    const raw = (
      await this.guard(() => this.run(buildGetSecretArgs(secretNameFor(subject), this.namespace)))
    ).trim();
    if (!raw) return null; // --ignore-not-found prints nothing
    return parseSecretJson(raw);
  }

  /** Idempotent: the create is attempted every time and AlreadyExists is the expected outcome. */
  private async ensureSecret(subject: string): Promise<void> {
    try {
      await this.run(buildCreateSecretArgs(secretNameFor(subject), this.namespace));
    } catch (err) {
      // AlreadyExists is the expected outcome and is NOT an outage; anything else is.
      if (!isAlreadyExists(err)) {
        if (err instanceof CpError) throw err;
        throw new CpError('credential_unavailable', 'the credential store is not answering');
      }
    }
  }

  async put(subject: string, cred: StoredCredential): Promise<void> {
    const { descriptor, secret } = cred;
    await this.ensureSecret(subject);
    const patch = {
      metadata: {
        annotations: {
          [annotationKey('kind', descriptor.name)]: descriptor.kind,
          [annotationKey('consumer', descriptor.name)]: descriptor.consumer,
          [annotationKey('destination', descriptor.name)]: JSON.stringify(
            descriptor.destination.hosts,
          ),
          [annotationKey('binding', descriptor.name)]: JSON.stringify(descriptor.binding),
          // A null clears a stale endpoint left by a previous, inference-flavoured put.
          [annotationKey('endpoint', descriptor.name)]: descriptor.endpoint,
        },
      },
      // stringData, so kubectl does the base64. Read back through `data`, which is base64 -- the
      // asymmetry is kubectl's, and the fake in the test reproduces it on purpose.
      stringData: {
        [descriptor.name]: seal(this.keks, subject, descriptor.name, JSON.stringify(secret)),
      },
    };
    await this.guard(() =>
      this.run(buildPatchSecretArgs(secretNameFor(subject), this.namespace), JSON.stringify(patch)),
    );
  }

  async get(subject: string, name: string): Promise<StoredCredential | null> {
    const secret = await this.fetch(subject);
    const sealed = secret?.data?.[name];
    const annotations = secret?.metadata?.annotations ?? {};
    if (!sealed) return null;
    const descriptor = describe(name, annotations);
    if (!descriptor) return null;
    // Throws when NO key in the ring opens it, or on a wrong subject or a relabelled ciphertext --
    // the AAD is what makes "move Alice's row into Bob's" fail rather than succeed (spec §6.5). A
    // value sealed under a retired KEK still opens while that key remains in the ring, which is what
    // makes rotation possible; one sealed under a key that has been dropped throws rather than
    // reading as absent.
    const opened = this.openOrBlameSubject(subject, name, sealed);
    return { descriptor, secret: JSON.parse(opened) as Record<string, string> };
  }

  /**
   * `open`, plus the two things an operator needs from it and a caller must never get.
   *
   * The rotation procedure's last step -- drop the retired KEK "once nothing is left under it" -- was
   * a step nobody could decide: `open` computed the ring index and discarded it, nothing counted a
   * non-primary open, `list()` never touches the KEK, `/v1` has no read-back path to sweep with, and
   * the audit record carries the decision but not the key. So the only signal that the key had been
   * dropped too early was the outage that followed. Both halves below exist to fix that:
   *
   * - `keyIndex > 0` is logged on EVERY read, deliberately not deduplicated. The terminating condition
   *   is "no credential has opened under a non-primary key for N days", and a once-per-process log
   *   would let a long-lived pod satisfy it while credentials were still stale. Silence in steady
   *   state is what makes it a signal; during a rotation the volume IS the backlog.
   * - On failure, the log gains the subject hash. `open`'s own message names the credential, but a
   *   credential name is user-chosen and collides freely across subjects -- `my-anthropic` is the
   *   obvious pick for everyone -- so without this an operator knew some users were broken and could
   *   not enumerate which. The hash is already this class's object-name input, so it discloses no
   *   login, and it reaches no caller: `writeError` reduces a non-CpError to a bare `internal_error`
   *   with no message at all.
   */
  private openOrBlameSubject(subject: string, name: string, sealed: string): string {
    // Annotated rather than inferred: an unannotated `let` is an evolving `any`, which would let a
    // wrong shape reach `.plaintext` unchecked.
    let opened: Opened;
    try {
      opened = open(this.keks, subject, name, Buffer.from(sealed, 'base64').toString('utf8'));
    } catch (err) {
      throw new Error(
        `failed to decrypt credential '${name}' for subject ${subjectHash(subject)}`,
        { cause: err },
      );
    }
    if (opened.keyIndex > 0) {
      console.warn(
        `[control-plane] credential opened under NON-PRIMARY KEK ring index ${opened.keyIndex}: ` +
          `subject=${subjectHash(subject)} credential=${name} -- re-seals on its next PUT; ` +
          `the retired key cannot be dropped yet`,
      );
    }
    return opened.plaintext;
  }

  /** Descriptors only, from annotations -- so this path never touches the KEK (spec §6.2). */
  async list(subject: string): Promise<CredentialDescriptor[]> {
    const secret = await this.fetch(subject);
    if (!secret) return [];
    const annotations = secret.metadata?.annotations ?? {};
    const out: CredentialDescriptor[] = [];
    for (const name of Object.keys(secret.data ?? {})) {
      const descriptor = describe(name, annotations);
      // A stray data key with no annotations is skipped rather than reported with an invented
      // descriptor: list() reports only what it can actually describe.
      if (descriptor) out.push(descriptor);
    }
    return out.sort((a, b) => a.name.localeCompare(b.name));
  }

  async delete(subject: string, name: string): Promise<void> {
    const secret = await this.fetch(subject);
    if (!secret) return; // nothing to do, and creating one to delete from would be absurd
    const patch = {
      metadata: {
        annotations: Object.fromEntries(FIELDS.map((f) => [annotationKey(f, name), null])),
      },
      stringData: { [name]: null },
    };
    await this.guard(() =>
      this.run(buildPatchSecretArgs(secretNameFor(subject), this.namespace), JSON.stringify(patch)),
    );
  }
}

/** Rebuild a descriptor from annotations, or null when the required ones are missing/corrupt. */
function describe(name: string, annotations: Record<string, string>): CredentialDescriptor | null {
  const kind = annotations[annotationKey('kind', name)];
  const consumer = annotations[annotationKey('consumer', name)];
  const destination = annotations[annotationKey('destination', name)];
  const binding = annotations[annotationKey('binding', name)];
  if (!kind || !consumer || !destination || !binding) return null;
  try {
    return {
      name,
      kind,
      consumer: consumer as Consumer,
      destination: { hosts: JSON.parse(destination) as string[] },
      binding: JSON.parse(binding) as CredentialDescriptor['binding'],
      endpoint: annotations[annotationKey('endpoint', name)] ?? null,
    };
  } catch {
    return null;
  }
}
