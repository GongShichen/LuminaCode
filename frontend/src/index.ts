#!/usr/bin/env node
import { backendBin, ensureBackend, parseLaunchOptions, runBackendPassthrough, shouldPassthrough } from "./backend";
import { RpcClient } from "./rpc";
import { LuminaTui } from "./tui";

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  if (shouldPassthrough(args)) {
    runBackendPassthrough(args);
    return;
  }
  const ws = await ensureBackend();
  const rpc = new RpcClient(ws);
  await rpc.call("backend.status");
  const tui = new LuminaTui(rpc, parseLaunchOptions(args));
  await tui.start();
}

main().catch((err) => {
  const backendHint = `backend: ${backendBin()}`;
  console.error(`${err instanceof Error ? err.message : String(err)}\n${backendHint}`);
  process.exit(1);
});
