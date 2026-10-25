const assert = require("node:assert/strict");
const test = require("node:test");
const { LuminaTui } = require("../dist/tui.js");

test("permission cancellation closes only its owning modal and clears handlers", () => {
  const tui = Object.create(LuminaTui.prototype);
  let hidden = 0;
  tui.sessionID = "session";
  tui.permissionRequestID = "current";
  tui.modalKeyHandlers = new Map([["enter", () => assert.fail("stale reply")]]);
  tui.modal = { hide: () => hidden++ };
  tui.input = { focus: () => {} };
  tui.requestRender = () => {};
  const cancel = (request_id) => tui.handlePush({
    type: "event", session_id: "session",
    event: { type: "permission_cancelled", payload: { request_id } },
  });
  cancel("previous");
  assert.equal(hidden, 0);
  assert.equal(tui.permissionRequestID, "current");
  cancel("current");
  assert.equal(hidden, 1);
  assert.equal(tui.permissionRequestID, "");
  assert.equal(tui.modalKeyHandlers.size, 0);
});
