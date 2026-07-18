import os from "node:os";
import path from "node:path";

export interface PathEnvironment {
  LUMINA_APP_ROOT?: string;
  LOCALAPPDATA?: string;
}

export function appRoot(
  platform = process.platform,
  env: PathEnvironment = process.env,
  homeDir = os.homedir(),
): string {
  const pathApi = platform === "win32" ? path.win32 : path.posix;
  const override = (env.LUMINA_APP_ROOT || "").trim();
  if (override) {
    const expanded = override === "~"
      ? homeDir
      : override.startsWith("~/") || override.startsWith("~\\")
        ? pathApi.join(homeDir, override.slice(2))
        : override;
    if (!pathApi.isAbsolute(expanded)) {
      throw new Error(`LUMINA_APP_ROOT must be absolute: ${expanded}`);
    }
    const normalized = pathApi.normalize(expanded);
    if (normalized === pathApi.parse(normalized).root) {
      throw new Error(`Refusing unsafe LuminaCode AppRoot: ${normalized}`);
    }
    return normalized;
  }
  if (platform === "win32" && (env.LOCALAPPDATA || "").trim()) {
    const resolved = pathApi.join((env.LOCALAPPDATA || "").trim(), "LuminaCode");
    if (!pathApi.isAbsolute(resolved)) {
      throw new Error(`Resolved LuminaCode AppRoot must be absolute: ${resolved}`);
    }
    return resolved;
  }
  if (!homeDir.trim()) {
    throw new Error("Cannot resolve LuminaCode AppRoot: user home is empty");
  }
  const resolved = pathApi.join(homeDir, ".lumina");
  if (!pathApi.isAbsolute(resolved)) {
    throw new Error(`Resolved LuminaCode AppRoot must be absolute: ${resolved}`);
  }
  return resolved;
}

export function endpointPath(): string {
  return path.join(appRoot(), "state", "run", "backend.json");
}

export function backendLogPath(): string {
  return path.join(appRoot(), "state", "logs", "backend.log");
}
