import { createClient, type RedisClientType } from 'redis';

export interface SandboxRecord {
  sandboxId: string;
  labels: Record<string, string>;
  capabilities: string[];
  capacityMax: number;
  transport: 'grpc';
}

export interface RecordStore {
  put(rec: SandboxRecord): Promise<void>;
  remove(sandboxId: string): Promise<void>;
  list(): Promise<SandboxRecord[]>;
}

/** Redis hash of grpc presence records: field = sandboxId, value = JSON(SandboxRecord). */
export function recordsKey(): string {
  return 'sh:sandbox:records';
}

/**
 * node-redis-backed record store. Connects lazily; reuses REDIS_URL.
 *
 * Two things here are load-bearing and were each a real failure:
 *
 * 1. THE `error` LISTENER. A node-redis client is an EventEmitter, and an `'error'` event with
 *    no listener kills the process. A *rejected* `connect()` is safe on the pinned redis ^6
 *    (probed), which is why most of this repo omits the listener — but a socket that connected
 *    and LATER closes emits `'error'` on the client, and that is fatal. Observed twice: once
 *    when a redis container was recreated under a running relay, and again on an E11 density
 *    run, where the relay bound its port, a worker attached, and then the relay died on
 *    `SocketClosedUnexpectedlyError` from `RedisSocket.#onSocketError`. The E11 case is the
 *    one that made this worth fixing rather than documenting: those drivers start the relay
 *    themselves and nothing restarts it, so a redis blip is a lost measurement run, and the
 *    driver saw only a client-side timeout minutes later.
 *
 *    The listener deliberately does NOT swallow failure. node-redis rejects in-flight and
 *    queued commands when the socket drops, so `put`/`remove`/`list` still reject and their
 *    callers still see the error — the listener only stops the process-level crash while the
 *    client's own reconnect brings the socket back.
 *
 * 2. THE CONNECT IS RETRYABLE. `ready` used to be one memoised promise from a single
 *    `connect()`, so if that first attempt failed the promise stayed rejected for the life of
 *    the process and every later call failed even after redis came back. That is a live race
 *    for the experiment drivers, which start redis with `docker run -d` (returns as soon as
 *    the container exists, not when it accepts) and then immediately start the relay. Now a
 *    failed attempt clears the memo so the next call reconnects.
 */
export class RedisRecordStore implements RecordStore {
  private client: RedisClientType;
  private ready: Promise<void> | undefined;
  private readonly url: string;
  private readonly maxReconnectAttempts: number;
  /**
   * maxReconnectAttempts is an optional seam, not a knob anyone is expected to set: the default
   * gives up after about six seconds, which comfortably covers a redis container starting or
   * being recreated while still failing loudly and soon. Tests pass a tiny value so they can
   * pin the "rejects rather than hangs" property in milliseconds instead of waiting out the
   * real backoff.
   */
  constructor(url = process.env.REDIS_URL ?? 'redis://127.0.0.1:6379', maxReconnectAttempts = 10) {
    this.url = url;
    this.maxReconnectAttempts = maxReconnectAttempts;
    this.client = this.build();
  }
  private build(): RedisClientType {
    // The reconnect bound is not tuning, it is the other half of the error listener below.
    // Probed directly on the pinned redis ^6: WITHOUT a listener, a connect() to a dead port
    // surfaces the failure as a rejection; WITH one, the listener consumes that error and
    // connect() stays pending, retrying, indefinitely (still pending at 15s). So adding the
    // listener alone would trade the crash it fixes for a silent hang -- worse to diagnose,
    // because the relay binds its port before any record call, so the hang shows up much
    // later as a client-side timeout with nothing in the relay's log.
    //
    // Bounded retry keeps both properties: a transient blip (a redis container recreated, or
    // `docker run -d` returning before redis accepts) reconnects on its own, while a redis
    // that is genuinely absent gives up and REJECTS, so the caller fails loudly and soon.
    const c = createClient({
      url: this.url,
      socket: {
        reconnectStrategy: (retries: number) =>
          retries > this.maxReconnectAttempts
            ? new Error(`redis at ${this.url} unreachable after ${retries} attempts`)
            : Math.min(retries * 100, 1000),
      },
    }) as RedisClientType;
    // Never rethrow from here: this handler exists precisely because an unhandled 'error'
    // exits the process. The failure still reaches callers through their own rejected
    // command, so logging is the whole job.
    c.on('error', (err: unknown) => {
      console.error(
        `RedisRecordStore: redis client error (commands will reject until it reconnects): ${String(err)}`,
      );
    });
    return c;
  }
  /**
   * Connect once, but do not memoise a FAILURE. On rejection the memo is cleared so the next
   * caller retries, instead of the store being permanently dead after one transient blip.
   */
  private connectOnce(): Promise<void> {
    if (!this.ready) {
      this.ready = this.client
        .connect()
        .then(() => undefined)
        .catch((err: unknown) => {
          this.ready = undefined;
          throw err;
        });
    }
    return this.ready;
  }
  async put(rec: SandboxRecord): Promise<void> {
    await this.connectOnce();
    await this.client.hSet(recordsKey(), rec.sandboxId, JSON.stringify(rec));
  }
  async remove(sandboxId: string): Promise<void> {
    await this.connectOnce();
    await this.client.hDel(recordsKey(), sandboxId);
  }
  async list(): Promise<SandboxRecord[]> {
    await this.connectOnce();
    const all = await this.client.hGetAll(recordsKey());
    return Object.values(all).map((v) => JSON.parse(v) as SandboxRecord);
  }
  async close(): Promise<void> {
    // close() must not resurrect a connection just to shut it down, and must not throw when
    // nothing was ever connected — a caller tearing down after a failed start is the normal
    // path, not an error.
    if (!this.ready) return;
    try {
      await this.ready;
    } catch {
      return;
    }
    await this.client.close();
  }
}
