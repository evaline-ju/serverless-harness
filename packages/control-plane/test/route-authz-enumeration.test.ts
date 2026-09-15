import { describe, expect, it } from 'vitest';
import { ROUTES } from '../src/routes.js';
import { HANDLERS } from '../src/handlers.js';
import { makeDeps, ctx, alice, bob, codeOf, seedCredential } from './helpers/deps.js';
import type { CpError } from '../src/errors.js';

describe('§9.3 test 2 — route-table enumeration', () => {
  it('gives every declared route a handler', () => {
    // `pnpm -r test` cannot notice a route declared and never wired: the router would 404 it and no
    // test would fail. This is the guard.
    const missing = ROUTES.filter((r) => typeof HANDLERS[r.operationId] !== 'function');
    expect(missing.map((r) => `${r.method} ${r.path}`)).toEqual([]);
  });

  it('gives every handler a declared route', () => {
    // The other direction: a handler with no route is dead code that a reader will assume is reachable.
    const declared = new Set(ROUTES.map((r) => r.operationId));
    expect(Object.keys(HANDLERS).filter((op) => !declared.has(op))).toEqual([]);
  });

  it('makes EVERY session-scoped route reject a non-owner with session_not_found', async () => {
    // The failure mode designed against is authz scattered per-handler, where the fifth endpoint
    // someone adds forgets the check (spec §5.4). A sixth session route added without assertOwner
    // fails CI here instead of shipping.
    const sessionScoped = ROUTES.filter((r) => r.sessionScoped);
    expect(sessionScoped.length).toBeGreaterThan(3); // guards the guard: never an empty sweep

    for (const route of sessionScoped) {
      const d = makeDeps();
      await seedCredential(d);
      await HANDLERS.createSession!(ctx({ principal: alice, body: {} }), d);
      const code = await codeOf(() =>
        HANDLERS[route.operationId]!(
          ctx({ principal: bob, params: { id: 'sid-fixed' }, body: {} }),
          d,
        ),
      );
      expect(code, `${route.method} ${route.path} did not 404 a non-owner`).toBe(
        'session_not_found',
      );
    }
  });

  it('makes every session-scoped route reject a missing principal', async () => {
    for (const route of ROUTES.filter((r) => r.sessionScoped)) {
      const d = makeDeps();
      const code = await codeOf(() =>
        HANDLERS[route.operationId]!(ctx({ params: { id: 'sid-fixed' }, body: {} }), d),
      );
      expect(code, `${route.method} ${route.path} ran with no principal`).toBe('token_required');
    }
  });

  it('makes every api-scoped non-session route reject a missing principal', async () => {
    const routes = ROUTES.filter((r) => r.auth === 'api' && !r.sessionScoped);
    expect(routes.length).toBeGreaterThan(2);
    for (const route of routes) {
      const d = makeDeps();
      const code = await codeOf(() =>
        HANDLERS[route.operationId]!(ctx({ params: { name: 'k' }, body: {} }), d),
      );
      expect(code, `${route.method} ${route.path} ran with no principal`).toBe('token_required');
    }
  });
});

describe('cross-tenant negatives (spec §9.3)', () => {
  it("A's token cannot drive B's sid, and B's list omits A's sessions", async () => {
    const d = makeDeps();
    await seedCredential(d, 'github:1234');
    await seedCredential(d, 'github:9999');
    await HANDLERS.createSession!(ctx({ principal: alice, body: {} }), {
      ...d,
      newId: () => 'a-1',
    });
    await HANDLERS.createSession!(ctx({ principal: bob, body: {} }), { ...d, newId: () => 'b-1' });

    expect(
      await codeOf(() => HANDLERS.getSession!(ctx({ principal: alice, params: { id: 'b-1' } }), d)),
    ).toBe('session_not_found');
    const bobList = await HANDLERS.listSessions!(ctx({ principal: bob }), d);
    expect(
      (bobList.body as { sessions: { sessionId: string }[] }).sessions.map((s) => s.sessionId),
    ).toEqual(['b-1']);
  });

  it("A cannot delete B's session, and B's session survives the attempt", async () => {
    const d = makeDeps();
    await seedCredential(d, 'github:9999');
    await HANDLERS.createSession!(ctx({ principal: bob, body: {} }), { ...d, newId: () => 'b-1' });
    expect(
      await codeOf(() =>
        HANDLERS.deleteSession!(ctx({ principal: alice, params: { id: 'b-1' } }), d),
      ),
    ).toBe('session_not_found');
    expect(await d.index.get('b-1')).not.toBeNull();
  });
});
