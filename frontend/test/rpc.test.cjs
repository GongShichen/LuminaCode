const assert = require("node:assert/strict");
const { EventEmitter } = require("node:events");
const test = require("node:test");

const { RpcClient } = require("../dist/rpc.js");

class MockWebSocket extends EventEmitter {
  constructor(responder) {
    super();
    this.readyState = 1;
    this.sent = [];
    this.responder = responder;
  }

  send(value) {
    const request = JSON.parse(value);
    this.sent.push(request);
    this.responder?.(request, this);
  }

  respond(response) {
    queueMicrotask(() => this.emit("message", Buffer.from(JSON.stringify(response))));
  }

  close() {
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.emit("close", 1006, Buffer.from("test disconnect"));
  }

  ping() {}
}

test("RpcClient uses UUIDs and retries a protocol-v3 mutation with the same id", async () => {
  const first = new MockWebSocket((request, socket) => {
    if (request.method === "backend.status") {
      socket.respond({ id: request.id, ok: true, result: { protocol_version: 3 } });
    }
  });
  let replacement;
  const rpc = new RpcClient(first, async () => {
    replacement = new MockWebSocket((request, socket) => {
	  if (request.method === "backend.status") {
		socket.respond({ id: request.id, ok: true, result: { protocol_version: 3 } });
	  }
      if (request.method === "session.yolo") {
        socket.respond({ id: request.id, ok: true, result: { yolo: true } });
      }
    });
    return replacement;
  });
  await rpc.call("backend.status");
  const pending = rpc.call("session.yolo", { session_id: "session-1" });
  const original = first.sent.find((request) => request.method === "session.yolo");
  assert.match(original.id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i);
  first.close();
  assert.deepEqual(await pending, { yolo: true });
  const retried = replacement.sent.find((request) => request.method === "session.yolo");
  assert.equal(retried.id, original.id);
});
