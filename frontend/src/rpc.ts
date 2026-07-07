import type WebSocket from "ws";

import type { PushEvent, RpcResponse } from "./types";

export class RpcClient {
  private seq = 0;
  private pending = new Map<string, { resolve: (value: any) => void; reject: (err: Error) => void }>();
  private eventHandlers: Array<(event: PushEvent) => void> = [];
  private disconnectHandlers: Array<(reason: string) => void> = [];
  private heartbeat?: NodeJS.Timeout;
  private disconnected = false;

  constructor(private ws: WebSocket) {
    ws.on("message", (data) => {
      const msg = JSON.parse(data.toString());
      if (msg.type === "event") {
        this.eventHandlers.forEach((handler) => handler(msg));
        return;
      }
      const response = msg as RpcResponse;
      const waiter = this.pending.get(response.id);
      if (!waiter) return;
      this.pending.delete(response.id);
      if (response.ok) {
        waiter.resolve(response.result);
      } else {
        waiter.reject(new Error(`${response.error?.code || "rpc_error"}: ${response.error?.message || "unknown error"}`));
      }
    });
    ws.on("close", (code, reason) => {
      const message = `backend websocket closed${code ? ` (${code})` : ""}${reason?.length ? `: ${reason.toString()}` : ""}`;
      this.handleDisconnect(message);
    });
    ws.on("error", (err) => {
      this.handleDisconnect(`backend websocket error: ${err instanceof Error ? err.message : String(err)}`);
    });
    this.heartbeat = setInterval(() => {
      if (this.disconnected) return;
      if (this.ws.readyState !== 1) {
        this.handleDisconnect("backend websocket is not open");
        return;
      }
      try {
        (this.ws as any).ping?.();
      } catch (err) {
        this.handleDisconnect(`backend websocket ping failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    }, 30_000);
    this.heartbeat.unref?.();
  }

  onEvent(handler: (event: PushEvent) => void): void {
    this.eventHandlers.push(handler);
  }

  onDisconnect(handler: (reason: string) => void): void {
    this.disconnectHandlers.push(handler);
  }

  call(method: string, params: Record<string, any> = {}): Promise<any> {
    const id = `req_${++this.seq}`;
    if (this.disconnected || this.ws.readyState !== 1) {
      return Promise.reject(new Error("backend disconnected"));
    }
    this.ws.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
    });
  }

  private handleDisconnect(reason: string): void {
    if (this.disconnected) return;
    this.disconnected = true;
    if (this.heartbeat) {
      clearInterval(this.heartbeat);
      this.heartbeat = undefined;
    }
    const err = new Error(reason);
    for (const waiter of this.pending.values()) {
      waiter.reject(err);
    }
    this.pending.clear();
    for (const handler of this.disconnectHandlers) {
      handler(reason);
    }
  }
}
