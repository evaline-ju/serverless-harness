import { buildFindPodBySelectorArgs, buildGetPodPhaseArgs, type RunKubectl } from './kubectl.js';
import type { SessionRecord } from './ownership.js';

/**
 * The /resources projection (spec §7.4).
 *
 * There is no session -> pod index in the tree (spec §2.4: leases are a ZSET whose member is the RUN
 * id, not the session id), so the harness SELF-REPORTS into sh:cp:session:<sid>:runtime. That hash is
 * written by the brain tier, so it is untrusted, DISPLAY-ONLY data and is never consulted for authz
 * -- which is why this module emits only fields it knows by name rather than spreading the hash.
 *
 * Fail-soft throughout: for an introspection endpoint, partial data with explicit unknowns beats an
 * error (spec §9.2), so a Kubernetes outage yields `phase: "unknown"`, never a 500.
 */
export interface SandboxView {
  podName: string | null;
  phase: string;
  tenant: string | null;
}

export async function resolveSandbox(
  runtime: Record<string, string>,
  namespace: string,
  run: RunKubectl | undefined,
  tenant: string,
): Promise<SandboxView> {
  const pinned = runtime.sandboxPod;
  const selector = runtime.sandboxSelector;
  if (!run || (!pinned && !selector)) {
    return { podName: pinned ?? null, phase: 'unknown', tenant };
  }
  try {
    if (pinned) {
      const phase = (await run(buildGetPodPhaseArgs(pinned, namespace))).trim();
      return { podName: pinned, phase: phase || 'unknown', tenant };
    }
    const [name = '', phase = ''] = (await run(buildFindPodBySelectorArgs(selector!, namespace)))
      .trim()
      .split('\t');
    return { podName: name || null, phase: phase || 'unknown', tenant };
  } catch {
    // K8s API down, RBAC denied, kubectl missing -- all the same answer to a caller asking "where is
    // my session running": we do not know right now.
    return { podName: pinned ?? null, phase: 'unknown', tenant };
  }
}

const num = (v: string | undefined): number | null =>
  v !== undefined && Number.isFinite(Number(v)) ? Number(v) : null;

export function projectResources(
  rec: SessionRecord,
  runtime: Record<string, string>,
  sandbox: SandboxView,
): unknown {
  const harnessPod = runtime.harnessPod ?? null;
  return {
    session: {
      id: rec.sessionId,
      state: rec.state,
      createdAt: rec.createdAt,
      lastTurnAt: num(runtime.lastTurnAt),
      turns: num(runtime.turns) ?? 0,
    },
    harness: {
      // A leaf runs in a KEDA-spawned worker Job whose pod name starts with the ScaledJob name; a
      // Knative revision pod does not. Display-only, so a heuristic is the right cost here.
      mode: harnessPod?.startsWith('leaf-worker') ? 'leaf-job' : 'knative',
      podName: harnessPod,
      revision: runtime.revision ?? null,
      // Nothing self-reported means nothing is serving the session right now -- which is the normal
      // state of a scaled-to-zero Knative Service, not an error.
      ready: harnessPod !== null,
    },
    sandbox,
    // null, not a fabricated zero: MU1's /turn path takes no pool lease and has no queue position
    // (spec §8.2), so MU2 filling these in should be visibly a change.
    lease: runtime.leaseKey
      ? {
          key: runtime.leaseKey,
          runId: runtime.runId ?? null,
          expiresAt: null,
          ttlSeconds: null,
        }
      : null,
    queue: null,
  };
}
