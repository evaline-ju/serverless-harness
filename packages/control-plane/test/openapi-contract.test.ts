import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { ROUTES } from '../src/routes.js';
import { CP_ERROR_CODES } from '../src/errors.js';
import { CREDENTIAL_NAME_RE } from '../src/credential-store.js';

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const doc = () => parse(readFileSync(resolve(REPO_ROOT, 'docs/api/openapi.yaml'), 'utf8'));

type Operation = { operationId?: string; security?: { [k: string]: unknown }[] };

function operations(): { path: string; method: string; op: Operation }[] {
  const out: { path: string; method: string; op: Operation }[] = [];
  for (const [path, byMethod] of Object.entries(
    doc().paths as Record<string, Record<string, Operation>>,
  )) {
    for (const [method, op] of Object.entries(byMethod)) {
      if (['get', 'post', 'put', 'delete', 'patch'].includes(method)) {
        out.push({ path, method: method.toUpperCase(), op });
      }
    }
  }
  return out;
}

describe('§9.3 test 3 — contract drift', () => {
  it('is a valid-looking OpenAPI 3.1 document', () => {
    const d = doc();
    expect(String(d.openapi)).toMatch(/^3\.1/);
    expect(d.info?.title).toBeTruthy();
    expect(d.info?.version).toBeTruthy();
  });

  it('documents every implemented route', () => {
    const documented = new Set(operations().map((o) => `${o.method} ${o.path}`));
    const missing = ROUTES.map((r) => `${r.method} ${r.path}`).filter((k) => !documented.has(k));
    expect(missing, 'implemented but undocumented').toEqual([]);
  });

  it('implements every documented route', () => {
    const implemented = new Set(ROUTES.map((r) => `${r.method} ${r.path}`));
    const extra = operations()
      .map((o) => `${o.method} ${o.path}`)
      .filter((k) => !implemented.has(k));
    expect(extra, 'documented but not implemented').toEqual([]);
  });

  it('uses the same operationId on both sides', () => {
    // The operationId is the join key for everything above; a mismatch would let the two tables
    // describe different routes while both assertions passed.
    const byKey = new Map(ROUTES.map((r) => [`${r.method} ${r.path}`, r.operationId]));
    for (const { path, method, op } of operations()) {
      expect(op.operationId, `${method} ${path}`).toBe(byKey.get(`${method} ${path}`));
    }
  });

  it('marks the auth requirement of every route consistently with the route table', () => {
    const byKey = new Map(ROUTES.map((r) => [`${r.method} ${r.path}`, r.auth]));
    for (const { path, method, op } of operations()) {
      const auth = byKey.get(`${method} ${path}`);
      const schemes = (op.security ?? []).flatMap((s) => Object.keys(s));
      if (auth === 'none') expect(schemes, `${method} ${path}`).toEqual([]);
      if (auth === 'api') expect(schemes, `${method} ${path}`).toEqual(['sessionToken']);
      if (auth === 'exchange') expect(schemes, `${method} ${path}`).toEqual(['exchangeToken']);
    }
  });

  it('declares both security schemes it references', () => {
    expect(Object.keys(doc().components?.securitySchemes ?? {}).sort()).toEqual([
      'exchangeToken',
      'sessionToken',
    ]);
  });

  it('states the same credential-name pattern the code enforces', () => {
    // Two sources of truth for a validation rule is how a documented API starts lying.
    const param = doc().components.parameters.credentialName;
    expect(param.schema.pattern).toBe(CREDENTIAL_NAME_RE.source);
  });

  it('never describes a RESPONSE that returns a credential value', () => {
    // GET /v1/credentials is metadata only and there is no read-back path anywhere in /v1 (spec §4.2).
    // Scoped to responses on purpose: `PUT /v1/credentials/{name}` legitimately documents `secret` in
    // its REQUEST body -- that is the write-only path. Stringifying whole path objects would flag it.
    const v1Responses = Object.entries(doc().paths as Record<string, Record<string, unknown>>)
      .filter(([path]) => path.startsWith('/v1/'))
      .flatMap(([, byMethod]) =>
        Object.values(byMethod).map((op) => (op as { responses?: unknown }).responses),
      );
    const json = JSON.stringify(v1Responses);
    expect(json).not.toContain('secret');
    expect(json).not.toContain('anthropicAuthToken');
  });

  it('documents EXACTLY the error codes the taxonomy can emit -- no more, no fewer', () => {
    // Set equality against the code's own list, not a transcribed subset, and against the machine-
    // readable `enum` rather than the file's raw text. Both halves matter:
    //   - a hand-listed subset silently stops covering code #19 the day it is added, while still
    //     looking like it covers the taxonomy;
    //   - a raw-text `toContain` search passes on any mention ANYWHERE in the document -- including a
    //     prose `description:` paragraph -- so it would stay green with no error responses documented
    //     at all.
    // Asserting both directions also catches the reverse drift: a code documented that the code
    // cannot actually emit, which sends integrators down a branch that never happens.
    const enumerated = doc().components.schemas.Error.properties.error.enum as string[];
    expect([...enumerated].sort()).toEqual([...CP_ERROR_CODES].sort());
  });
});
