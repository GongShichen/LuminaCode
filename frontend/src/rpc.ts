import { randomUUID } from "node:crypto";
import type WebSocket from "ws";

import type { PushEvent, RpcResponse, RuntimeEvent } from "./types";
import { delay } from "./utils";

type PendingRequest = {
  id: string;
  method: string;
  params: Record<string, any>;
  internal: boolean;
  resolve: (value: any) => void;
  reject: (err: Error) => void;
};

export class RpcClient {
  private ws: WebSocket;
  private generation = 0;
  private pending = new Map<string, PendingRequest>();
  private eventHandlers: Array<(event: PushEvent) => void> = [];
  private disconnectHandlers: Array<(reason: string) => void> = [];
  private recoveryHandlers: Array<(snapshot: any) => void> = [];
  private heartbeat?: NodeJS.Timeout;
  private reconnecting = false;
  private terminallyDisconnected = false;
  private protocolVersion = 0;
  private sessionID = "";
  private sessionCWD = "";
  private lastSeq = 0;

  constructor(ws: WebSocket, private reconnect?: () => Promise<WebSocket>) {
    this.ws = ws;
    this.attach(ws);
  }

  onEvent(handler: (event: PushEvent) => void): void {
    this.eventHandlers.push(handler);
  }

  onDisconnect(handler: (reason: string) => void): void {
    this.disconnectHandlers.push(handler);
  }

  onRecovery(handler: (snapshot: any) => void): void {
    this.recoveryHandlers.push(handler);
  }

  call(method: string, params: Record<string, any> = {}): Promise<any> {
    const id = randomUUID();
    if (method === "session.create" && !params.session_id) {
      params = { ...params, session_id: id };
    }
	if (method.startsWith("team.") && this.isMutation(method) && !params.command_id) {
	  params = { ...params, command_id: id };
	}
    if (this.terminallyDisconnected) {
      return Promise.reject(new Error("backend disconnected"));
    }
    return new Promise((resolve, reject) => {
      const request: PendingRequest = { id, method, params, internal: false, resolve, reject };
      this.pending.set(id, request);
      if (this.ws.readyState === 1 && !this.reconnecting) {
        this.send(request);
      }
    });
  }

  private attach(ws: WebSocket): void {
    this.ws = ws;
    const generation = ++this.generation;
    ws.on("message", (data) => {
      if (generation !== this.generation) return;
      let msg: any;
      try {
        msg = JSON.parse(data.toString());
      } catch {
        return;
      }
      if (msg.type === "event") {
        this.trackEvent(msg as PushEvent);
        this.eventHandlers.forEach((handler) => handler(msg as PushEvent));
        return;
      }
      const response = msg as RpcResponse;
      const waiter = this.pending.get(response.id);
      if (!waiter) return;
      this.pending.delete(response.id);
      if (response.ok) {
		if (!waiter.internal) this.trackResult(waiter.method, waiter.params, response.result);
        waiter.resolve(response.result);
      } else {
        waiter.reject(new Error(`${response.error?.code || "rpc_error"}: ${response.error?.message || "unknown error"}`));
      }
    });
    ws.on("close", (code, reason) => {
      if (generation !== this.generation) return;
      const message = `backend websocket closed${code ? ` (${code})` : ""}${reason?.length ? `: ${reason.toString()}` : ""}`;
      this.handleSocketLoss(message);
    });
    ws.on("error", (err) => {
      if (generation !== this.generation) return;
      this.handleSocketLoss(`backend websocket error: ${err instanceof Error ? err.message : String(err)}`);
    });
    this.startHeartbeat(generation);
  }

  private startHeartbeat(generation: number): void {
    if (this.heartbeat) clearInterval(this.heartbeat);
    this.heartbeat = setInterval(() => {
      if (generation !== this.generation || this.reconnecting || this.terminallyDisconnected) return;
      if (this.ws.readyState !== 1) {
        this.handleSocketLoss("backend websocket is not open");
        return;
      }
      try {
        (this.ws as any).ping?.();
      } catch (err) {
        this.handleSocketLoss(`backend websocket ping failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    }, 30_000);
    this.heartbeat.unref?.();
  }

  private send(request: PendingRequest): void {
    if (this.ws.readyState !== 1) return;
    this.ws.send(JSON.stringify({ id: request.id, method: request.method, params: request.params }));
  }

  private handleSocketLoss(reason: string): void {
	if (this.terminallyDisconnected) return;
    if (this.heartbeat) {
      clearInterval(this.heartbeat);
      this.heartbeat = undefined;
    }
    for (const [id, request] of this.pending) {
      if (request.internal) {
        request.reject(new Error(reason));
        this.pending.delete(id);
      }
    }
	if (this.reconnecting) return;
    if (!this.reconnect) {
      this.failPermanently(reason);
      return;
    }
    this.reconnecting = true;
    void this.reconnectLoop(reason);
  }

  private async reconnectLoop(initialReason: string): Promise<void> {
    const started = Date.now();
    const backoff = [250, 500, 1_000, 2_000, 5_000];
    let attempt = 0;
    let lastReason = initialReason;
    while (Date.now() - started < 30_000 && this.reconnect) {
      await delay(backoff[Math.min(attempt, backoff.length - 1)]);
      attempt += 1;
      try {
        const ws = await this.reconnect();
        this.attach(ws);
        await this.recoverSession();
        this.reconnecting = false;
        for (const request of this.pending.values()) {
          if (request.internal) continue;
          if (this.isMutation(request.method) && this.protocolVersion < 3) {
            request.reject(new Error("retry_unsafe: unconfirmed mutation requires protocol v3 UUID idempotency"));
            this.pending.delete(request.id);
            continue;
          }
          this.send(request);
        }
        return;
      } catch (err) {
        lastReason = err instanceof Error ? err.message : String(err);
        try {
          this.ws.close();
        } catch {
          // Best effort between retry attempts.
        }
      }
    }
    this.failPermanently(`${initialReason}; reconnect failed: ${lastReason}`);
  }

  private async recoverSession(): Promise<void> {
	const status = await this.requestOnce("backend.status", {});
	this.protocolVersion = Number(status?.protocol_version || 0);
    if (!this.sessionID) return;
    const previousSeq = this.lastSeq;
    const snapshot = await this.requestOnce("session.resume", {
      session_id: this.sessionID,
      cwd: this.sessionCWD,
    });
    let afterSeq = previousSeq;
    for (;;) {
      const page = await this.requestOnce("session.events", {
        session_id: this.sessionID,
        after_seq: afterSeq,
        limit: 500,
      });
      const events = Array.isArray(page?.events) ? page.events : [];
      for (const event of events) this.emitRecoveredEvent(event as RuntimeEvent);
      afterSeq = Number(page?.next_after_seq || afterSeq);
      if (!page?.has_more || events.length === 0) break;
    }
    this.lastSeq = Math.max(afterSeq, Number(snapshot?.last_seq || 0));
    for (const handler of this.recoveryHandlers) handler(snapshot);
  }

  private requestOnce(method: string, params: Record<string, any>): Promise<any> {
    const id = randomUUID();
    return new Promise((resolve, reject) => {
      const request: PendingRequest = { id, method, params, internal: true, resolve, reject };
      this.pending.set(id, request);
      this.send(request);
    });
  }

  private emitRecoveredEvent(event: RuntimeEvent): void {
    const push: PushEvent = {
      type: "event",
      protocol_version: 3,
      session_id: event.session_id || this.sessionID,
      stream_id: event.stream_id,
      seq: event.seq,
      event_id: event.event_id,
      event_type: event.type,
      schema_version: event.schema_version,
      durable: true,
      timestamp: event.occurred_at,
      payload: event.payload,
      event: { type: "runtime.event", payload: event },
    };
    this.trackEvent(push);
    for (const handler of this.eventHandlers) handler(push);
  }

  private trackEvent(event: PushEvent): void {
    if (event.session_id) this.sessionID = event.session_id;
    if (event.durable && event.seq) this.lastSeq = Math.max(this.lastSeq, Number(event.seq));
  }

  private trackResult(method: string, params: Record<string, any>, result: any): void {
    if (method === "backend.status") {
      this.protocolVersion = Number(result?.protocol_version || 0);
    }
    if (method === "session.create" || method === "session.resume" || method === "session.snapshot") {
      this.sessionID = String(result?.session_id || params.session_id || this.sessionID);
      this.sessionCWD = String(result?.cwd || params.cwd || this.sessionCWD);
      this.lastSeq = Math.max(this.lastSeq, Number(result?.last_seq || 0));
    }
  }

  private isMutation(method: string): boolean {
    return !new Set([
      "backend.status",
      "session.list",
      "session.snapshot",
      "session.events",
      "session.tokens",
      "runtime.describe",
      "storage.status",
      "team.list",
      "team.snapshot",
      "team.status",
      "team.artifacts",
      "team.timeline",
      "team.dialogue",
      "team.summary",
      "team.detail",
      "memory.search",
      "memory.doctor",
      "slash.list",
      "skills.list",
      "mcp.list",
    ]).has(method);
  }

  private failPermanently(reason: string): void {
    if (this.terminallyDisconnected) return;
    this.reconnecting = false;
    this.terminallyDisconnected = true;
    if (this.heartbeat) clearInterval(this.heartbeat);
    const error = new Error(reason);
    for (const request of this.pending.values()) request.reject(error);
    this.pending.clear();
    for (const handler of this.disconnectHandlers) handler(reason);
  }
}
