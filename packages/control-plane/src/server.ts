import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { CpError, writeError } from './errors.js';
import { checkExchangeAuth } from './exchange.js';
import { HANDLERS, type CpDeps, type RequestCtx } from './handlers.js';
import { matchRoute, type RouteSpec } from './routes.js';
import { verifyToken } from './token.js';

const JSON_HEADERS = { 'Content-Type': 'application/json' };
/** A credential body is a few hundred bytes; 64 KiB is generous and bounds a hostile caller. */
const MAX_BODY_BYTES = 64 * 1024;

function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    let tooLarge = false;
    req.on('data', (chunk: Buffer) => {
      size += chunk.length;
      // Stop buffering rather than keep accumulating: an unbounded read is a trivial memory
      // exhaustion on a service that is by design always on. We still drain (rather than
      // `req.destroy()`) so the socket survives long enough to carry the error response back --
      // destroying the request mid-stream races the still-unread bytes sitting in the kernel's
      // receive buffer and the OS answers with an abortive RST (ECONNRESET) instead of delivering
      // the 400 the caller is waiting to read.
      if (size > MAX_BODY_BYTES) {
        tooLarge = true;
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (tooLarge) {
        reject(new CpError('invalid_request', 'request body too large'));
        return;
      }
      resolve(Buffer.concat(chunks).toString());
    });
    req.on('error', reject);
  });
}

const bearer = (req: IncomingMessage): string | undefined => {
  const header = req.headers.authorization;
  if (!header) return undefined;
  const [scheme, ...rest] = header.split(' ');
  if (scheme?.toLowerCase() !== 'bearer') return undefined;
  const value = rest.join(' ').trim();
  return value.length > 0 ? value : undefined;
};

/**
 * Authenticate per the ROUTE's declared kind, not per handler. The router is the only layer that sees
 * headers, so it is the only place this can live -- and driving it off RouteSpec.auth is what makes
 * "which routes need a token" enumerable by a test rather than scattered through handler bodies.
 */
function authorize(route: RouteSpec, req: IncomingMessage, deps: CpDeps, ctx: RequestCtx): void {
  if (route.auth === 'none') return;
  const presented = bearer(req);
  if (route.auth === 'exchange') {
    // Throws CpError('unauthorized') with no mention of the presented value (spec §5.3.1).
    checkExchangeAuth(presented, deps.config.exchangeToken);
    ctx.exchangeAuthorized = true;
    return;
  }
  if (!presented) throw new CpError('token_required', 'this route requires a token');
  // requiredScope 'api' is what stops a session token rewriting credentials or creating a second
  // session, and stops the shared exchange bearer standing in for a user identity (plan gap #7).
  ctx.principal = verifyToken(presented, deps.verifyKeys, {
    now: Math.floor(deps.now() / 1000),
    requiredScope: 'api',
  });
}

export function buildHandler(deps: CpDeps): (req: IncomingMessage, res: ServerResponse) => void {
  return (req, res) => {
    const route = async () => {
      const matched = matchRoute(req.method ?? '', req.url ?? '');
      if (!matched) {
        res.writeHead(404, JSON_HEADERS).end(JSON.stringify({ error: 'not_found' }));
        return;
      }
      const url = new URL(req.url ?? '/', 'http://localhost');
      const ctx: RequestCtx = { params: matched.params, query: url.searchParams, body: undefined };

      // AUTHENTICATION FIRST, then the body. `authorize` reads only headers, so the order is free --
      // and with it reversed an unauthenticated `PUT /v1/credentials/x` carrying malformed JSON got
      // 400 invalid_json instead of 401, telling an anonymous caller something about the route's body
      // handling before it had established the caller may talk to the route at all. Nothing of
      // consequence leaked and the 64 KiB cap bounded it, but authentication belongs in front. Every
      // route here is new in MU1, so no existing caller depends on the old codes.
      authorize(matched.route, req, deps, ctx);

      if (req.method === 'POST' || req.method === 'PUT') {
        const raw = await readBody(req);
        if (raw.length > 0) {
          try {
            ctx.body = JSON.parse(raw);
          } catch {
            throw new CpError('invalid_json', 'body is not valid JSON');
          }
        } else if (matched.route.operationId !== 'startDeviceAuth') {
          // startDeviceAuth legitimately takes no body; every other POST/PUT needs one, and an empty
          // body must be a 400 rather than an `undefined` a handler then misreads as `{}`.
          throw new CpError('invalid_json', 'body is required');
        }
      }

      const handler = HANDLERS[matched.route.operationId];
      // A declared-but-unwired route is a programming error; the enumeration test catches it in CI,
      // and this is the runtime answer if one ever slips through.
      if (!handler) throw new CpError('internal_error', 'route has no handler');

      const { status, body } = await handler(ctx, deps);
      if (body === undefined) {
        res.writeHead(status).end();
        return;
      }
      if (typeof body === 'string') {
        res.writeHead(status, { 'Content-Type': 'text/plain' }).end(body);
        return;
      }
      res.writeHead(status, JSON_HEADERS).end(JSON.stringify(body));
    };

    route().catch((err) => {
      // Every error leaves through writeError, which maps a CpError to its status and reduces anything
      // else to a bare `internal_error` -- so a Redis URL with a password in it cannot reach a caller.
      if (!(err instanceof CpError)) console.error('[control-plane] unhandled', err);
      writeError(res, err);
    });
  };
}

export function startControlPlane(deps: CpDeps, port = 8080): Server {
  const server = createServer(buildHandler(deps));
  const onSigterm = () => {
    server.close(() => process.exit(0));
  };
  process.on('SIGTERM', onSigterm);
  // A production process calls this once; a test suite calls it once per test. Without removing the
  // listener on close, every server started this way leaks one 'SIGTERM' handler on `process` for the
  // life of the process, tripping Node's MaxListenersExceededWarning well before a real suite's tenth
  // test and masking a genuine future leak behind expected noise.
  server.once('close', () => process.removeListener('SIGTERM', onSigterm));
  server.listen(port, () => {
    const addr = server.address();
    const bound = typeof addr === 'object' && addr ? addr.port : port;
    console.log(`sh-control-plane listening on :${bound}`);
  });
  return server;
}
