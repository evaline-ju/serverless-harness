/**
 * The control plane's typed boundary errors. Codes are `snake_case` and bodies are
 * `{ error, message?, sessionId? }`, matching the data plane's existing `invalid_json` /
 * `session_not_found` / `prompt_required` (packages/knative-server/src/server.ts). Mapped to a
 * status in exactly one place, so a handler never picks a status itself (spec §9.1).
 */
/**
 * Every error code the control plane can emit, as a VALUE so tests can iterate it.
 *
 * The type derives from this array rather than being declared beside it, which makes the two
 * impossible to disagree: adding a code here forces a `STATUS` entry (Record exhaustiveness) and
 * forces the OpenAPI `Error.error` enum to list it (openapi-contract.test.ts). Order is irrelevant --
 * every consumer sorts.
 */
export const CP_ERROR_CODES = [
  'invalid_json',
  'invalid_request',
  'token_required',
  'token_invalid',
  'token_expired',
  'subject_conflict',
  'unauthorized',
  'forbidden',
  'session_not_found',
  'session_mismatch',
  'credential_required',
  'credential_ambiguous',
  'credential_not_found',
  'credential_unavailable',
  'endpoint_unresolved',
  'authorization_pending',
  'redis_unavailable',
  'internal_error',
] as const;

export type CpErrorCode = (typeof CP_ERROR_CODES)[number];

const STATUS: Record<CpErrorCode, number> = {
  invalid_json: 400,
  invalid_request: 400,
  token_required: 401,
  token_invalid: 401,
  token_expired: 401,
  // A request carrying both a session token and a conflicting X-SH-Subject is REJECTED rather than
  // resolved by precedence -- a silent winner here is a cross-tenant bug waiting (spec §3.5).
  subject_conflict: 400,
  unauthorized: 401,
  forbidden: 403,
  // 404, never 403, for another user's session: 403 is an existence oracle (spec §8.1). 403 is
  // reserved for "authenticated but insufficiently privileged on a resource you may know exists".
  session_not_found: 404,
  session_mismatch: 400,
  credential_required: 400,
  credential_ambiguous: 400,
  credential_not_found: 404,
  credential_unavailable: 503,
  endpoint_unresolved: 400,
  // 428 Precondition Required: the device-flow poll has a distinct status so a polling client needs
  // no body inspection to tell "not yet" from "denied" (plan gap #8).
  authorization_pending: 428,
  redis_unavailable: 503,
  internal_error: 500,
};

const JSON_HEADERS = { 'Content-Type': 'application/json' };

export class CpError extends Error {
  constructor(
    readonly code: CpErrorCode,
    message?: string,
    readonly sessionId?: string,
  ) {
    super(message ?? code);
    this.name = 'CpError';
  }
}

export function statusFor(code: CpErrorCode): number {
  return STATUS[code];
}

/** Minimal structural response surface -- lets tests assert without a live http server. */
interface WritableRes {
  headersSent: boolean;
  writeHead(status: number, headers?: unknown): unknown;
  end(body?: string): unknown;
}

/**
 * Terminate a request with a typed error. An unrecognised throwable becomes a bare
 * `internal_error` with NO message: an arbitrary error's text can carry a Redis connection string
 * or a presented token, and this body is returned to an arbitrary caller.
 */
export function writeError(res: WritableRes, err: unknown): void {
  if (res.headersSent) return;
  if (err instanceof CpError) {
    const body: Record<string, string> = { error: err.code };
    if (err.message && err.message !== err.code) body.message = err.message;
    if (err.sessionId) body.sessionId = err.sessionId;
    res.writeHead(statusFor(err.code), JSON_HEADERS);
    res.end(JSON.stringify(body));
    return;
  }
  res.writeHead(500, JSON_HEADERS);
  res.end(JSON.stringify({ error: 'internal_error' }));
}
