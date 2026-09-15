import type { CpRedisLike } from '../../src/ownership.js';

/**
 * In-memory CpRedisLike. Reproduces node-redis's REV argument order -- (key, max, min) when REV is
 * set -- on purpose: a fake that accepted either order would let a reversed page ship.
 */
export function fakeRedis() {
  const hashes = new Map<string, Record<string, string>>();
  const zsets = new Map<string, { score: number; value: string }[]>();
  const streams = new Map<string, Record<string, string>[]>();
  const ops: string[] = [];
  const redis: CpRedisLike = {
    async hSet(key, values) {
      ops.push(`hSet ${key}`);
      hashes.set(key, { ...(hashes.get(key) ?? {}), ...values });
      return 1;
    },
    async hGetAll(key) {
      return { ...(hashes.get(key) ?? {}) };
    },
    async zAdd(key, member) {
      ops.push(`zAdd ${key}`);
      const z = zsets.get(key) ?? [];
      zsets.set(key, [...z.filter((m) => m.value !== member.value), member]);
      return 1;
    },
    async zRem(key, member) {
      ops.push(`zRem ${key}`);
      zsets.set(
        key,
        (zsets.get(key) ?? []).filter((m) => m.value !== member),
      );
      return 1;
    },
    async zRange(key, max, min, opts) {
      if (!opts?.REV || opts.BY !== 'SCORE') throw new Error('this fake only serves REV BY SCORE');
      const parse = (v: number | string) => {
        const s = String(v);
        if (s === '+inf') return { bound: Infinity, exclusive: false };
        if (s === '-inf') return { bound: -Infinity, exclusive: false };
        return s.startsWith('(')
          ? { bound: Number(s.slice(1)), exclusive: true }
          : { bound: Number(s), exclusive: false };
      };
      const hi = parse(max);
      const lo = parse(min);
      let rows = [...(zsets.get(key) ?? [])]
        .filter((m) => (hi.exclusive ? m.score < hi.bound : m.score <= hi.bound))
        .filter((m) => (lo.exclusive ? m.score > lo.bound : m.score >= lo.bound))
        .sort((a, b) => b.score - a.score);
      if (opts.LIMIT) rows = rows.slice(opts.LIMIT.offset, opts.LIMIT.offset + opts.LIMIT.count);
      return rows.map((m) => m.value);
    },
    async del(keys) {
      ops.push(`del ${keys.join(',')}`);
      for (const k of keys) {
        hashes.delete(k);
        zsets.delete(k);
      }
      return keys.length;
    },
    async zScore(key, member) {
      return (zsets.get(key) ?? []).find((m) => m.value === member)?.score ?? null;
    },
    async xAdd(key, _id, fields) {
      ops.push(`xAdd ${key}`);
      streams.set(key, [...(streams.get(key) ?? []), fields]);
      return '1-0';
    },
  };
  return { redis, hashes, zsets, streams, ops };
}
