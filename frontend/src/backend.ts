import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import WebSocket from "ws";

import type { LaunchOptions } from "./types";
import { delay } from "./utils";

export function parseLaunchOptions(args: string[]): LaunchOptions {
  let cwd = process.cwd();
  let resumeSessionID: string | undefined;
  for (let i = 0; i < args.length; i += 1) {
    const arg = args[i];
    if (arg === "--cwd" && args[i + 1]) {
      cwd = path.resolve(args[i + 1]);
      i += 1;
      continue;
    }
    if (arg.startsWith("--cwd=")) {
      cwd = path.resolve(arg.slice("--cwd=".length));
      continue;
    }
    if ((arg === "--resume" || arg === "-resume") && args[i + 1]) {
      resumeSessionID = args[i + 1];
      i += 1;
      continue;
    }
    if (arg.startsWith("--resume=")) {
      resumeSessionID = arg.slice("--resume=".length);
    }
  }
  return { cwd, resumeSessionID };
}

export function shouldPassthrough(args: string[]): boolean {
  if (args[0] === "daemon") return true;
  return args.some((arg) => {
    return (
      arg === "-p" ||
      arg === "--prompt" ||
      arg === "--list" ||
      arg === "--help" ||
      arg === "-h" ||
      arg.startsWith("-p=") ||
      arg.startsWith("--prompt=")
    );
  });
}

export function backendBin(): string {
  if (process.env.LUMINA_BACKEND_BIN) return process.env.LUMINA_BACKEND_BIN;
  const searchDirs = [__dirname];
  const argvScript = process.argv[1] || "";
  if (argvScript) searchDirs.push(path.dirname(argvScript));
  for (const dir of searchDirs) {
    for (const name of backendExecutableNames()) {
      const candidate = path.join(dir, name);
      if (fs.existsSync(candidate)) return candidate;
    }
  }
  return process.platform === "win32" ? "lumina-backend.exe" : "lumina-backend";
}

function backendExecutableNames(): string[] {
  if (process.platform === "win32") {
    return ["lumina-backend.exe", "lumina-backend"];
  }
  return ["lumina-backend", "lumina-backend.exe"];
}

export function runBackendPassthrough(args: string[]): void {
  const child = spawn(backendBin(), args, { stdio: "inherit" });
  child.on("error", (err) => {
    console.error(err.message);
    process.exit(1);
  });
  child.on("exit", (code) => process.exit(code ?? 0));
}

export async function ensureBackend(): Promise<WebSocket> {
  const existing = readEndpoint();
  if (existing?.port && existing?.auth_token) {
    try {
      return await connectEndpoint(existing);
    } catch {
      // Fall through and start a fresh backend.
    }
  }
  fs.mkdirSync(path.dirname(endpointPath()), { recursive: true });
  const before = Date.now();
  const child = spawn(backendBin(), ["daemon", "--host", "127.0.0.1", "--port", "0"], {
    detached: true,
    stdio: "ignore",
  });
  child.unref();
  for (let i = 0; i < 80; i += 1) {
    await delay(100);
    const info = readEndpoint();
    if (!info?.port || !info?.auth_token) continue;
    const stat = fs.statSync(endpointPath());
    if (stat.mtimeMs + 500 < before) continue;
    try {
      return await connectEndpoint(info, 1200);
    } catch {
      // Keep polling.
    }
  }
  throw new Error("Unable to start lumina-backend daemon");
}

function endpointPath(): string {
  return path.join(os.homedir(), ".lumina", "run", "backend.json");
}

function readEndpoint(): any | null {
  try {
    return JSON.parse(fs.readFileSync(endpointPath(), "utf8"));
  } catch {
    return null;
  }
}

function connectEndpoint(info: any, timeoutMs = 700): Promise<WebSocket> {
  return new Promise((resolve, reject) => {
    const url = `ws://${info.host || "127.0.0.1"}:${info.port}/v1/ws?token=${encodeURIComponent(info.auth_token)}`;
    const ws = new WebSocket(url);
    const timer = setTimeout(() => {
      ws.close();
      reject(new Error("connect timeout"));
    }, timeoutMs);
    ws.once("open", () => {
      clearTimeout(timer);
      resolve(ws);
    });
    ws.once("error", (err) => {
      clearTimeout(timer);
      reject(err);
    });
  });
}
