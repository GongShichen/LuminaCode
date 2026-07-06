import type WebSocket from "ws";

import type { PushEvent, RpcResponse } from "./types";

export class RpcClient {
  private seq = 0;
  private pending = new Map<string, { resolve: (value: any) => void; reject: (err: Error) => void }>();
  private eventHandlers: Array<(event: PushEvent) => void> = [];

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
  }

  onEvent(handler: (event: PushEvent) => void): void {
    this.eventHandlers.push(handler);
  }

  call(method: string, params: Record<string, any> = {}): Promise<any> {
    const id = `req_${++this.seq}`;
    this.ws.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
    });
  }
}
