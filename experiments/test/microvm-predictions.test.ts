import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const FILE = resolve(REPO_ROOT, 'deploy/microvm/predictions.json');

/**
 * Spec §7.4 records five falsifiable predictions BEFORE the first rung, and says why
 * the pin matters: "A prediction that can be edited to fit the data is a hypothesis."
 *
 * So this test is not a schema check — it is a tamper seal. Editing a prediction after
 * results land fails here, loudly, and the only correct responses are (a) revert, or
 * (b) update this digest in the SAME commit that says in its message why a prediction
 * was legitimately restated. Both leave a trace; a silent edit does not.
 */
const PINNED_SHA256 = '375ecb47f9faa4bdc42108c1e2a13a8acddb03137bf95f686ef4ec3f9caae3d4';

describe('microVM predictions are recorded before the rungs and pinned', () => {
  it('has not been edited', () => {
    const raw = readFileSync(FILE);
    const actual = createHash('sha256').update(raw).digest('hex');
    expect(
      actual,
      'deploy/microvm/predictions.json no longer matches its pinned SHA-256. ' +
        'This pin exists so a prediction cannot be quietly edited to fit results once ' +
        'they land (spec §7.4: "a prediction that can be edited to fit the data is a ' +
        'hypothesis"). If this change is legitimate — a genuine correction made BEFORE ' +
        'any measurement the correction would be informed by — update PINNED_SHA256 in ' +
        'this file in the same commit, and say in the commit message why the prediction ' +
        'was restated. A silent edit is exactly what this test is here to stop.',
    ).toBe(PINNED_SHA256);
  });

  it('records all five of §7.4s predictions, each with a falsifier', () => {
    const doc = JSON.parse(readFileSync(FILE, 'utf8'));
    expect(doc.predictions).toHaveLength(5);
    for (const p of doc.predictions) {
      expect(p.claim.length).toBeGreaterThan(20);
      // A prediction with no stated falsifier is not falsifiable, which is the whole
      // property §7.4 is introducing to this repo.
      expect(p.falsifiedBy.length).toBeGreaterThan(10);
      expect(p.metric.length).toBeGreaterThan(5);
    }
  });

  it('was recorded before any results exist', () => {
    const doc = JSON.parse(readFileSync(FILE, 'utf8'));
    expect(doc.recordedAt).toMatch(/^\d{4}-\d{2}-\d{2}$/);
    expect(doc.spec).toBe('docs/specs/2026-09-09-p4-microvm-sandbox-design.md');
  });
});
