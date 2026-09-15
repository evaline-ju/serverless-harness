import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse, parseAllDocuments } from 'yaml';

type EnvVar = {
  name: string;
  value?: string;
  valueFrom?: { secretKeyRef?: { name: string; key: string; optional?: boolean } };
};

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const DEPLOY = resolve(REPO_ROOT, 'deploy/knative');
const docs = (file: string) =>
  parseAllDocuments(readFileSync(resolve(DEPLOY, file), 'utf8')).map((d) => d.toJS());
const cp = () => docs('control-plane.yaml');
const container = () =>
  cp().find((o) => o.kind === 'Deployment').spec.template.spec.containers[0] as {
    env: EnvVar[];
    workingDir?: string;
    command?: string[];
    securityContext?: Record<string, unknown>;
    readinessProbe?: { httpGet?: { path?: string } };
  };
const envOf = (name: string) => container().env.find((e) => e.name === name);

describe('control-plane.yaml shape', () => {
  it('is a plain Deployment, not a Knative Service', () => {
    // It holds a signing key, mints tokens for cron-fired runs with no client present, and is the
    // trusted tier -- scale-to-zero would buy nothing and cost a cold start on every list (spec §3.1).
    const all = cp();
    expect(all.some((o) => o.apiVersion === 'serving.knative.dev/v1')).toBe(false);
    const dep = all.find((o) => o.kind === 'Deployment');
    expect(dep.metadata.name).toBe('sh-control-plane');
    expect(dep.spec.replicas).toBe(1);
  });

  it('ships a Namespace, a ServiceAccount and a Service alongside it', () => {
    const kinds = cp().map((o) => o.kind);
    expect(kinds).toContain('Namespace');
    expect(kinds).toContain('ServiceAccount');
    expect(kinds).toContain('Service');
    expect(cp().find((o) => o.kind === 'Namespace').metadata.name).toBe('sh-credentials');
  });

  it('runs its own package entrypoint, so tsx and redis resolve (relay precedent)', () => {
    const c = container();
    expect(c.workingDir).toBe('/app/packages/control-plane');
    expect(c.command!.join(' ')).toBe('node --import tsx src/main.ts');
  });

  it('is hardened the way the harness Service is', () => {
    const c = container();
    expect(c.securityContext).toMatchObject({
      allowPrivilegeEscalation: false,
      readOnlyRootFilesystem: true,
    });
    expect((c.securityContext!.capabilities as { drop: string[] }).drop).toEqual(['ALL']);
    expect(c.readinessProbe?.httpGet?.path).toBe('/readyz');
  });
});

describe('the credential-store RBAC is the containment (spec §6.5)', () => {
  const roles = () => cp().filter((o) => o.kind === 'Role');
  const runtimeRole = () => roles().find((r) => r.metadata.name === 'sh-control-plane-credentials');
  const maintenanceRole = () =>
    roles().find((r) => r.metadata.name === 'sh-control-plane-credentials-maintenance');

  it('grants the serving path get/create/update/patch/delete on secrets and OMITS list', () => {
    // The Secret name is derived from the subject, so every access is a get by exact name. Without
    // `list`, a bug or an injection cannot enumerate users' credential objects -- reaching Bob's
    // Secret requires already knowing Bob's subject.
    const rule = runtimeRole().rules.find((r: { resources: string[] }) =>
      r.resources.includes('secrets'),
    );
    expect([...rule.verbs].sort()).toEqual(['create', 'delete', 'get', 'patch', 'update']);
    expect(rule.verbs).not.toContain('list');
    expect(rule.verbs).not.toContain('watch');
  });

  it('scopes that Role to the dedicated namespace, which is what makes the containment possible', () => {
    // Kubernetes RBAC filters by resourceNames, never by label -- so "read the credential store" and
    // "read any Secret in the app namespace" can only be different permissions if the store has its
    // own namespace.
    expect(runtimeRole().metadata.namespace).toBe('sh-credentials');
  });

  it('keeps list in a separate maintenance Role that is bound to nothing', () => {
    // Cleanup of departed users needs list, so it goes to a maintenance Role used by a Job -- never to
    // the serving path (spec §6.5).
    expect(maintenanceRole().rules[0].verbs).toContain('list');
    const boundRoles = cp()
      .filter((o) => o.kind === 'RoleBinding')
      .map((b) => b.roleRef.name);
    expect(boundRoles).not.toContain('sh-control-plane-credentials-maintenance');
    expect(boundRoles).toContain('sh-control-plane-credentials');
  });

  it('grants pods get/list only, and only in the workload namespace', () => {
    // /resources reads pod phase (spec §7.4). Pods are not secrets: listing them enumerates no users.
    const podRole = roles().find((r) =>
      r.rules.some((x: { resources: string[] }) => x.resources.includes('pods')),
    );
    expect(podRole.metadata.namespace).toBe('default');
    const rule = podRole.rules.find((r: { resources: string[] }) => r.resources.includes('pods'));
    expect([...rule.verbs].sort()).toEqual(['get', 'list']);
  });
});

describe('control-plane env', () => {
  it('sources the signing key, the KEK and the exchange token from Secrets, never literals', () => {
    for (const name of ['SH_SESSION_TOKEN_PRIVATE_KEY', 'SH_CREDENTIAL_KEK', 'SH_EXCHANGE_TOKEN']) {
      const v = envOf(name);
      expect(v?.valueFrom?.secretKeyRef, name).toBeTruthy();
      expect(v?.value, `${name} must not be a literal`).toBeUndefined();
    }
  });

  it('mounts the KEK from a DIFFERENT Secret than anything else', () => {
    // A namespace-wide secret read must yield ciphertext, and the KEK must be a distinct RBAC subject
    // (spec §6.5) -- which it cannot be if it shares an object with the data it opens.
    const kek = envOf('SH_CREDENTIAL_KEK')!.valueFrom!.secretKeyRef!.name;
    const signing = envOf('SH_SESSION_TOKEN_PRIVATE_KEY')!.valueFrom!.secretKeyRef!.name;
    expect(kek).not.toBe(signing);
  });

  it('defaults the operator fallback off', () => {
    // spec §6.4: SH_ALLOW_OPERATOR_FALLBACK defaults false, so a deployment does not silently let one
    // subject spend the operator's key.
    expect(envOf('SH_ALLOW_OPERATOR_FALLBACK')?.value).toBe('false');
  });

  it('carries the Redis URL and the credential namespace', () => {
    expect(envOf('REDIS_URL')?.value).toContain('redis');
    expect(envOf('SH_CREDENTIAL_NAMESPACE')?.value).toBe('sh-credentials');
  });

  it('declares SH_GITHUB_CLIENT_ID as an empty placeholder the operator must fill in', () => {
    // The device flow treats the app as a public client, so there is no client secret to protect -- but
    // the app must be registered by hand with device flow enabled (spec §5.1.1), so no checked-in value
    // can be correct. Present-but-empty beats omitted: it documents the name and shows up in
    // `kubectl set env`, and main.ts's `required()` guard rejects '' exactly as it rejects unset, so
    // either way the pod fails at STARTUP rather than serving broken logins. The manifest comment says
    // so out loud, because "must be filled in" reads as degraded-but-running and this is not that.
    expect(envOf('SH_GITHUB_CLIENT_ID')).toBeTruthy();
    expect(envOf('SH_GITHUB_CLIENT_ID')?.value).toBe('');
  });
});

describe('the data plane learns about the control plane', () => {
  // service.yaml holds FOUR documents (Service, ServiceAccount, Role, RoleBinding), so `parse()`
  // throws "Source contains multiple documents" on it -- use the same multi-doc helper as
  // control-plane.yaml and select the Knative Service on BOTH apiVersion and kind, since a core/v1
  // Service would otherwise match on kind alone.
  const svc = () =>
    docs('service.yaml').find(
      (o) => o.apiVersion === 'serving.knative.dev/v1' && o.kind === 'Service',
    );
  const svcEnv = (): EnvVar[] => svc().spec.template.spec.containers[0].env;

  it('defaults SH_REQUIRE_AUTH to false', () => {
    // 14 scripts in deploy/knative call /turn or /runs with no auth today; making the token mandatory
    // in one step breaks every smoke path and both demos (spec §4.3.1). Flipping it is MU2.
    expect(svcEnv().find((e) => e.name === 'SH_REQUIRE_AUTH')?.value).toBe('false');
  });

  it('takes the public keyset as plain configuration, because a public key is not a secret', () => {
    const keys = svcEnv().find((e) => e.name === 'SH_SESSION_TOKEN_PUBLIC_KEYS');
    expect(keys).toBeTruthy();
    expect(keys?.valueFrom).toBeUndefined();
  });

  it('points at the control-plane Service and mounts the shared exchange token from a Secret', () => {
    expect(svcEnv().find((e) => e.name === 'SH_CONTROL_PLANE_URL')?.value).toContain(
      'sh-control-plane',
    );
    const ex = svcEnv().find((e) => e.name === 'SH_EXCHANGE_TOKEN');
    expect(ex?.valueFrom?.secretKeyRef?.name).toBe('sh-exchange-token');
    // optional:true so the base manifest still applies on a cluster that has not created the Secret --
    // the exchange is fail-closed in code, so an absent value rejects every call rather than opening one
    // (plan gap #3).
    expect(ex?.valueFrom?.secretKeyRef?.optional).toBe(true);
  });

  it('reads the same Secret name on both sides, or the exchange 401s every turn', () => {
    const dataPlane = svcEnv().find((e) => e.name === 'SH_EXCHANGE_TOKEN')!.valueFrom!
      .secretKeyRef!;
    const controlPlane = envOf('SH_EXCHANGE_TOKEN')!.valueFrom!.secretKeyRef!;
    expect(controlPlane.name).toBe(dataPlane.name);
    expect(controlPlane.key).toBe(dataPlane.key);
  });

  describe('SH_REQUIRE_AUTH=true must not ship while the control plane is collocated (spec §8.2)', () => {
    // The control plane holds the Ed25519 session-signing key, the credential KEK and the shared
    // exchange token, all as env vars (see 'control-plane env' above). It runs in namespace `default`
    // today, where the harness ServiceAccount holds `pods/exec: ['create']` with no `resourceNames`
    // restriction (service.yaml:113-127, control-plane.yaml:138 and :200-214) -- so anyone with code
    // execution in the harness pod can `kubectl exec` into sh-control-plane and read all three from
    // /proc/1/environ. Either half below is a legitimate state on its own -- today ships 'false' +
    // 'default', the intended end state is 'true' + 'sh-credentials' -- only the combination is
    // dangerous, so the assertion below must be a conjunction of values read from the manifests.
    const requireAuth = () => svcEnv().find((e) => e.name === 'SH_REQUIRE_AUTH')?.value;
    const controlPlaneNamespace = () =>
      cp().find((o) => o.kind === 'Deployment').metadata.namespace;

    it('refuses SH_REQUIRE_AUTH=true while the control plane still runs in the workload namespace', () => {
      const authIsLive = requireAuth() === 'true';
      const collocatedWithWorkload = controlPlaneNamespace() === 'default';
      expect(
        authIsLive && collocatedWithWorkload,
        'SH_REQUIRE_AUTH=true with sh-control-plane still deployed in `default` exposes the ' +
          'session-signing key, the credential KEK and the shared exchange token to anyone with code ' +
          'execution in the harness pod: `kubectl exec` into sh-control-plane, then read ' +
          '/proc/1/environ -- defeating the "the harness cannot mint a token" property. Fix: move the ' +
          'control plane to its own namespace (sh-credentials) before flipping this flag, and when you ' +
          'do, also add a namespaceSelector to the egress policy above or the exchange hop breaks with ' +
          'a 503. See spec §8.2.',
      ).toBe(false);
    });

    // Canary pinning the current shipped state. This is EXPECTED to fail the moment either half
    // changes -- update it in the same change that moves the namespace or flips the flag, so a
    // namespace move that forgot the flag (or a flag flip that forgot the namespace) still shows up
    // as a failure here, even on the rare occasion the conjunction above stays green.
    it('canary: pins the current shipped state (false + default)', () => {
      expect(requireAuth()).toBe('false');
      expect(controlPlaneNamespace()).toBe('default');
    });
  });
});

describe('rollout posture', () => {
  it('is NOT in the base kustomization — MU1 ships opt-in', () => {
    // Making the control plane part of the default stack is MU2, once SH_REQUIRE_AUTH flips (spec §10).
    // `parse()` is correct HERE: kustomization.yaml is a single document. service.yaml is not -- see
    // the helper above.
    const base = parse(readFileSync(resolve(DEPLOY, 'kustomization.yaml'), 'utf8'));
    expect(base.resources).not.toContain('control-plane.yaml');
  });
});

describe('the harness can actually reach the control plane (egress composition)', () => {
  // harness-egress-policy.yaml is default-deny egress on the harness pod and its allowlist predates
  // MU1 -- DNS, Redis 6379, relay 8443, 0.0.0.0/0 on 443/6443. The exchange hop is TCP 8080, which no
  // rule there permits, so on any egress-enforcing cluster every authenticated turn ends in
  // 503 credential_unavailable. control-plane.yaml therefore ships an ADDITIVE policy (policies union),
  // and the base file stays untouched because MU1 is opt-in.
  type NetPol = {
    kind: string;
    metadata: { namespace?: string };
    spec: {
      podSelector: { matchLabels?: Record<string, string> };
      policyTypes?: string[];
      egress?: {
        to?: { podSelector?: { matchLabels?: Record<string, string> } }[];
        ports?: { protocol?: string; port?: number }[];
      }[];
    };
  };
  const netpols = (file: string) =>
    docs(file).filter((o) => o.kind === 'NetworkPolicy') as NetPol[];
  // The one policy in control-plane.yaml that opens a path to the control plane's own pod labels.
  const cpLabels = () =>
    cp().find((o) => o.kind === 'Deployment').spec.template.metadata.labels as Record<
      string,
      string
    >;
  const added = () => {
    const pols = netpols('control-plane.yaml');
    expect(pols.length, 'control-plane.yaml must ship an egress policy for the exchange hop').toBe(
      1,
    );
    return pols[0];
  };
  // Parsed out of the URL rather than hardcoded, so a future SH_CONTROL_PLANE_URL port change fails
  // here instead of in production.
  const exchangePort = () => {
    const url = (
      docs('service.yaml').find(
        (o) => o.apiVersion === 'serving.knative.dev/v1' && o.kind === 'Service',
      ).spec.template.spec.containers[0].env as EnvVar[]
    ).find((e) => e.name === 'SH_CONTROL_PLANE_URL')?.value;
    expect(url, 'SH_CONTROL_PLANE_URL must be set on the harness Service').toBeTruthy();
    const port = Number(new URL(url!).port);
    expect(Number.isInteger(port) && port > 0, `no explicit port in ${url}`).toBe(true);
    return port;
  };

  it('allows egress to the control-plane pod on the exchange port, in the workload namespace', () => {
    const pol = added();
    expect(pol.metadata.namespace).toBe('default');
    expect(pol.spec.policyTypes).toEqual(['Egress']);
    const rule = pol.spec.egress!.find((e) =>
      e.to?.some((t) => t.podSelector?.matchLabels?.app === cpLabels().app),
    );
    expect(rule, "no egress rule selects the control plane's own pod labels").toBeTruthy();
    expect(rule!.ports).toEqual([{ protocol: 'TCP', port: exchangePort() }]);
  });

  it('selects the harness pod exactly the way the base default-deny policy does', () => {
    // The composition assertion: two independently authored files, compared. A drift here means the
    // added policy selects nothing and the base policy still denies 8080 -- silently, until a turn runs.
    const base = netpols('harness-egress-policy.yaml');
    expect(base.length).toBe(1);
    expect(added().spec.podSelector).toEqual(base[0].spec.podSelector);
  });

  it('does not widen the base allowlist — the exchange port is absent from it', () => {
    // If someone "helpfully" edits harness-egress-policy.yaml instead, opting out of MU1 stops being
    // "do not apply control-plane.yaml" and every base deployment gains the hop.
    const basePorts = netpols('harness-egress-policy.yaml')[0]
      .spec.egress!.flatMap((e) => e.ports ?? [])
      .map((p) => p.port);
    expect(basePorts).not.toContain(exchangePort());
  });
});
