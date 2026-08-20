import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import WebSocket from "ws";

import type { LaunchOptions } from "./types";
import { backendLogPath, endpointPath } from "./paths";
import { delay } from "./utils";

export interface BackendConnection {
  ws: WebSocket;
  reconnect: () => Promise<WebSocket>;
}

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
  if (["daemon", "shutdown", "layout", "memory"].includes(args[0] || "")) return true;
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
	return (await ensureBackendConnection()).ws;
}

export async function ensureBackendConnection(): Promise<BackendConnection> {
  const configuredURL = process.env.LUMINA_BACKEND_URL?.trim();
  if (configuredURL) {
    const reconnect = () => connectURL(normalizeBackendURL(configuredURL), 5_000);
    return { ws: await reconnect(), reconnect };
  }
  const existing = readEndpoint();
	if (existing?.port && (existing?.auth_token || bearerToken())) {
    try {
	  const reconnect = () => connectPreferredLocalEndpoint();
	  return { ws: await connectEndpoint(existing), reconnect };
    } catch (err) {
	  if (!existing.auth_token) throw err;
      // Fall through and start a fresh backend.
    }
  }
  fs.mkdirSync(path.dirname(endpointPath()), { recursive: true, mode: 0o700 });
  fs.mkdirSync(path.dirname(backendLogPath()), { recursive: true, mode: 0o700 });
  const logPath = backendLogPath();
  const logFd = fs.openSync(logPath, "a", 0o600);
  fs.writeSync(logFd, `\n--- lumina-backend start ${new Date().toISOString()} ---\n`);
  const before = Date.now();
  const child = spawn(backendBin(), ["daemon", "--host", "127.0.0.1", "--port", "0"], {
    detached: true,
    stdio: ["ignore", logFd, logFd],
  });
  child.on("error", (err) => {
    try {
      fs.writeSync(logFd, `spawn error: ${err instanceof Error ? err.stack || err.message : String(err)}\n`);
    } catch {
      // Best effort logging only.
    }
  });
  child.unref();
  for (let i = 0; i < 80; i += 1) {
    await delay(100);
    const info = readEndpoint();
	if (!info?.port || (!info?.auth_token && !bearerToken())) continue;
    const stat = fs.statSync(endpointPath());
    if (stat.mtimeMs + 500 < before) continue;
    try {
	  const reconnect = () => connectPreferredLocalEndpoint();
	  return { ws: await connectEndpoint(info, 1200), reconnect };
    } catch {
      // Keep polling.
    }
  }
  throw new Error("Unable to start lumina-backend daemon");
}

async function connectPreferredLocalEndpoint(): Promise<WebSocket> {
  const info = readEndpoint();
	if (!info?.port || (!info?.auth_token && !bearerToken())) throw new Error("backend endpoint is unavailable");
  return connectEndpoint(info, 1_200);
}

function readEndpoint(): any | null {
  try {
    return JSON.parse(fs.readFileSync(endpointPath(), "utf8"));
  } catch {
    return null;
  }
}

function connectEndpoint(info: any, timeoutMs = 700): Promise<WebSocket> {
	let url = `ws://${info.host || "127.0.0.1"}:${info.port}/v1/ws`;
	if (info.auth_token) url += `?token=${encodeURIComponent(info.auth_token)}`;
	return connectURL(url, timeoutMs);
}

function connectURL(url: string, timeoutMs: number): Promise<WebSocket> {
  return new Promise((resolve, reject) => {
	const token = bearerToken();
	const ws = new WebSocket(url, token ? { headers: { Authorization: `Bearer ${token}` } } : undefined);
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

function bearerToken(): string | undefined {
	return process.env.LUMINA_JWT?.trim() || process.env.LUMINA_ACCESS_TOKEN?.trim();
}

function normalizeBackendURL(value: string): string {
  const parsed = new URL(value);
  if (parsed.protocol === "http:") parsed.protocol = "ws:";
  if (parsed.protocol === "https:") parsed.protocol = "wss:";
  if (parsed.protocol !== "ws:" && parsed.protocol !== "wss:") {
    throw new Error("LUMINA_BACKEND_URL must use http(s) or ws(s)");
  }
  if (!parsed.pathname || parsed.pathname === "/") parsed.pathname = "/v1/ws";
  return parsed.toString();
}
