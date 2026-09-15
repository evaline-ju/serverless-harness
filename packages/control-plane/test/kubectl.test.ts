import { describe, expect, it } from 'vitest';
import {
  buildCreateSecretArgs,
  buildDeleteSecretArgs,
  buildFindPodBySelectorArgs,
  buildGetPodPhaseArgs,
  buildGetSecretArgs,
  buildPatchSecretArgs,
  isAlreadyExists,
} from '../src/kubectl.js';

describe('secret argv builders', () => {
  it('creates an empty Secret by exact name', () => {
    expect(buildCreateSecretArgs('sh-cred-abc', 'sh-credentials')).toEqual([
      'create',
      'secret',
      'generic',
      'sh-cred-abc',
      '-n',
      'sh-credentials',
    ]);
  });

  it('patches from stdin, never from argv', () => {
    const args = buildPatchSecretArgs('sh-cred-abc', 'sh-credentials');
    expect(args).toEqual([
      'patch',
      'secret',
      'sh-cred-abc',
      '-n',
      'sh-credentials',
      '--type=merge',
      '--patch-file=/dev/stdin',
    ]);
    // argv is readable via /proc/<pid>/cmdline by anything sharing the pod, so a `-p <json>`
    // carrying a user's provider key would be exposed to every process in the container.
    expect(args.join(' ')).not.toContain('-p ');
  });

  it('reads a Secret by exact name and tolerates absence', () => {
    expect(buildGetSecretArgs('sh-cred-abc', 'sh-credentials')).toEqual([
      'get',
      'secret',
      'sh-cred-abc',
      '-n',
      'sh-credentials',
      '-o',
      'json',
      '--ignore-not-found',
    ]);
  });

  it('deletes idempotently', () => {
    expect(buildDeleteSecretArgs('sh-cred-abc', 'sh-credentials')).toContain('--ignore-not-found');
  });

  it('never builds a Secret list', () => {
    // Spec §6.5: the runtime Role grants get/create/update/patch/delete and OMITS `list`, so a bug
    // or an injection cannot enumerate users' credential objects. That RBAC is only usable if no
    // code path needs list -- pinned here rather than discovered as a 403 in production.
    const secretCalls = [
      buildCreateSecretArgs('n', 'ns'),
      buildPatchSecretArgs('n', 'ns'),
      buildGetSecretArgs('n', 'ns'),
      buildDeleteSecretArgs('n', 'ns'),
    ];
    for (const args of secretCalls) {
      expect(args, args.join(' ')).toContain('n'); // the object is always named
      expect(
        args.some((a) => a === '-l' || a.startsWith('--selector')),
        args.join(' '),
      ).toBe(false);
      expect(args[1]).toMatch(/^secret$/); // never the plural collection form
    }
  });
});

describe('pod argv builders', () => {
  it('reads one pod phase, tolerating a deleted pod', () => {
    expect(buildGetPodPhaseArgs('sandbox-0-0', 'default')).toEqual([
      'get',
      'pod',
      'sandbox-0-0',
      '-n',
      'default',
      '-o',
      'jsonpath={.status.phase}',
      '--ignore-not-found',
    ]);
  });

  it('finds the first Running pod for a selector and returns name + phase', () => {
    const args = buildFindPodBySelectorArgs('sh.kagenti.io/sandbox-pool=default', 'default');
    expect(args.slice(0, 6)).toEqual([
      'get',
      'pods',
      '-n',
      'default',
      '-l',
      'sh.kagenti.io/sandbox-pool=default',
    ]);
    expect(args).toContain('--field-selector=status.phase=Running');
    expect(args.join(' ')).toContain('{.items[0].metadata.name}');
  });
});

describe('isAlreadyExists', () => {
  it('recognises the create-race error so put() can be idempotent', () => {
    expect(
      isAlreadyExists(
        new Error('Error from server (AlreadyExists): secrets "sh-cred-x" already exists'),
      ),
    ).toBe(true);
    expect(isAlreadyExists(new Error('Error from server (Forbidden): cannot create secrets'))).toBe(
      false,
    );
    expect(isAlreadyExists('not an error')).toBe(false);
    expect(isAlreadyExists(undefined)).toBe(false);
  });
});
