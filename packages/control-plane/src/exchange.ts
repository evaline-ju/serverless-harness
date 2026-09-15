import { timingSafeEqual } from 'node:crypto';
import { CpError } from './errors.js';
import type { CpDeps } from './handlers.js';
import { verifyToken } from './token.js';

/**
 * The per-turn credential exchange (spec §5.3). The data plane presents the token it was given; the
 * control plane hands back that subject's credential.
 *
 * This puts the control plane on the CONTROL path once per turn -- never on the data path. It sees no
 * prompt and no model output; the SSE stream stays direct from the Knative Service to the client.
 *
 * If the credential rode inside the token instead, a client-visible bearer string would contain a
 * provider key -- landing in browser storage, proxy logs and shell history. This keeps it server-side.
 */
export type CredentialMode = 'placeholder' | 'direct';

export interface ExchangeResponse {
  mode: CredentialMode;
  anthropicAuthToken: string;
  /** NEVER undefined -- see the endpoint_unresolved refusal below. */
  anthropicBaseUrl: string;
  sessionId: string;
  subject: string;
}

/**
 * An inert, subject-derived stand-in for the real credential. RC1's `static-inject` rewrites
 * `Bearer <placeholder>` to the real value from a mounted secret_dir (P5 §3.1-§3.2), so the exact
 * string an injector matches on is RC1/P5's to fix; MU1 guarantees only that it is inert and names
 * the subject, and this function is the single place to change when that is pinned.
 */
export function placeholderFor(subject: string): string {
  return `sh-placeholder-${subject}`;
}

/**
 * Shared-bearer auth for /internal/credentials (spec §5.3.1), reusing the pattern this repo already
 * runs for the relay and remote worker. FAIL-CLOSED: with no token configured, EVERY call is
 * rejected -- an unconfigured deployment must not accept anything.
 *
 * mTLS remains the target, and Z1 is what makes it cheap: once per-session SPIFFE identities exist,
 * the exchange authorizes on the peer's SVID and gains what a shared token cannot -- the CALLER
 * identified per session rather than per deployment. Until then this hop's weakness is that any code
 * in the harness pod can call the exchange, which is why it returns only the credential for the
 * subject named by a SIGNED token it cannot mint.
 */
export function checkExchangeAuth(
  presented: string | undefined,
  configured: string | undefined,
): void {
  // The message never contains the presented value: a wrong token must not be logged with it.
  const deny = () => {
    throw new CpError('unauthorized', 'exchange authentication failed');
  };
  if (!configured) deny();
  if (!presented) deny();
  const a = Buffer.from(presented!);
  const b = Buffer.from(configured!);
  // Constant-time, and length-checked first because timingSafeEqual throws on a length mismatch.
  if (a.length !== b.length || !timingSafeEqual(a, b)) deny();
}

export async function exchangeCredential(
  presentedToken: string,
  deps: CpDeps,
): Promise<ExchangeResponse> {
  // Only a SESSION token may drive a turn; an api token is rejected here (plan gap #7).
  const claims = verifyToken(presentedToken, deps.verifyKeys, {
    now: Math.floor(deps.now() / 1000),
    requiredScope: 'turn:write',
  });
  if (!claims.sid) throw new CpError('token_invalid', 'token names no session');

  const rec = await deps.index.get(claims.sid);
  // Owner mismatch and unknown session are the same 404-shaped answer: a valid token minted for one
  // subject must not exchange against another's session even though both facts are true separately.
  if (!rec || rec.owner !== claims.sub) {
    throw new CpError('session_not_found', undefined, claims.sid);
  }
  // The tombstone check is what stops a DELETED session starting a new turn (spec §5.3, §7.3).
  if (rec.tombstone) throw new CpError('session_not_found', undefined, claims.sid);

  const stored = rec.credentialName
    ? await deps.credentials.get(rec.owner, rec.credentialName)
    : null;

  let secretValue: string | undefined;
  let credentialName = rec.credentialName;
  let endpoint: string | null = null;
  let usedOperatorFallback = false;

  if (stored) {
    // The kind's SINGLE secret field is the credential value. That is true by construction, not by
    // coincidence: parseCredentialBody refuses `consumer: 'inference'` for any kind declaring more
    // than one secret field (credential-store.ts), so `basic` cannot reach here and send its
    // username upstream as the bearer. Insertion order is parseCredentialBody's loop over
    // spec.secretFields, so with exactly one field there is nothing to pick wrong.
    secretValue = Object.values(stored.secret)[0];
    endpoint = stored.descriptor.endpoint;
  } else if (deps.config.allowOperatorFallback && deps.config.operatorInferenceToken) {
    // The operator fallback relocates rather than disappearing (spec §6.4): resolved HERE, by the
    // trusted tier, attributable to a subject and logged -- never as an env fallback in the harness.
    secretValue = deps.config.operatorInferenceToken;
    credentialName = 'operator-fallback';
    usedOperatorFallback = true;
  }

  if (!secretValue) {
    // REFUSES rather than reaching for the deployment's own key. This is the second of the two policy
    // points that make MU1 fail closed before P5's sentinel lands (spec §3.5).
    throw new CpError(
      'credential_required',
      `subject has no usable inference credential '${rec.credentialName}'`,
      rec.sessionId,
    );
  }

  const baseUrl = endpoint ?? deps.config.defaultInferenceEndpoint;
  if (!baseUrl) {
    // Never returned undefined. run-turn.ts:313's `||` would fall through to the environment and,
    // failing that, applyModelGateway would return a model carrying Bearer <subject's token> with NO
    // baseUrl override -- sending one user's gateway token to the default Anthropic endpoint, where it
    // is neither valid nor intended to go. A credential whose destination cannot be resolved is not a
    // degraded request; it is a misdirected secret (spec §6.2).
    throw new CpError(
      'endpoint_unresolved',
      `credential '${credentialName}' has no endpoint and no deployment default is set`,
      rec.sessionId,
    );
  }

  // Placeholder mode WINS whenever the deployment has an injector, so adding one strictly narrows
  // what the harness may hold; direct mode is reachable only when none is configured, and MU3 deletes
  // it outright (spec §3.6).
  const mode: CredentialMode = deps.config.injectorConfigured ? 'placeholder' : 'direct';

  await deps.index.audit({
    subject: rec.owner,
    sessionId: rec.sessionId,
    credential: credentialName,
    decision: usedOperatorFallback ? 'operator_fallback_used' : 'credential_issued',
  });

  return {
    mode,
    anthropicAuthToken: mode === 'placeholder' ? placeholderFor(rec.owner) : secretValue,
    anthropicBaseUrl: baseUrl,
    sessionId: rec.sessionId,
    subject: rec.owner,
  };
}
