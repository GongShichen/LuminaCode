#!/usr/bin/env node

import fs from "node:fs";
import path from "node:path";

const emptyRemoteFields = {
  memory_bge_api_key: "",
  memory_bge_base_url: "",
  memory_bge_model: "",
  memory_reranker_enabled: false,
  memory_reranker_api_key: "",
  memory_reranker_base_url: "",
  memory_reranker_model: "",
};

function fail(message) {
  process.stderr.write(`Memory model configuration failed: ${message}\n`);
  process.exit(1);
}

function parseArgs(argv) {
  const result = { command: argv[2] || "provider" };
  for (let index = 3; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) fail(`invalid argument ${name || ""}`.trim());
    result[name.slice(2)] = value;
  }
  return result;
}

function parseUseAPI(value) {
  const normalized = String(value ?? "0").trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return true;
  if (["0", "false", "no", "off", ""].includes(normalized)) return false;
  fail("MEMORY_USE_API must be 0 or 1");
}

function readSettings(settingsPath) {
  if (!fs.existsSync(settingsPath)) return {};
  try {
    const parsed = JSON.parse(fs.readFileSync(settingsPath, "utf8"));
    if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") {
      fail(`${settingsPath} must contain a JSON object`);
    }
    return parsed;
  } catch (error) {
    fail(`could not parse ${settingsPath}: ${error.message}`);
  }
}

function resolvedSettings(current, useAPI) {
  const next = { ...emptyRemoteFields, ...current };
  next.memory_bge_provider = useAPI ? "openai_compatible" : "local";
  next.memory_reranker_provider = useAPI ? "openai_compatible" : "off";
  if (!useAPI) next.memory_reranker_enabled = false;
  if (!Object.hasOwn(current, "memory_reranker_provider")) {
    next.memory_reranker_enabled = false;
    next.memory_reranker_model = "";
  }
  return next;
}

function writeSettings(settingsPath, settings) {
  fs.mkdirSync(path.dirname(settingsPath), { recursive: true, mode: 0o700 });
  const temporary = `${settingsPath}.tmp-${process.pid}`;
  fs.writeFileSync(temporary, `${JSON.stringify(settings, null, 2)}\n`, { mode: 0o600 });
  fs.renameSync(temporary, settingsPath);
  fs.chmodSync(settingsPath, 0o600);
}

const args = parseArgs(process.argv);
const appRoot = path.resolve(args["app-root"] || path.join(process.env.HOME || ".", ".lumina"));
const settingsPath = path.join(appRoot, "config", "settings.json");
const current = readSettings(settingsPath);
const useAPI = String(args["use-api"] ?? "").trim()
  ? parseUseAPI(args["use-api"])
  : current.memory_bge_provider === "openai_compatible";
const settings = resolvedSettings(current, useAPI);

switch (args.command) {
  case "provider":
    process.stdout.write(`${settings.memory_bge_provider}\n`);
    break;
  case "validate":
    process.stdout.write(`  memory models: ${useAPI ? "remote API selected; local download skipped" : "local BGE-M3 selected"}\n`);
    break;
  case "write":
    writeSettings(settingsPath, settings);
    process.stdout.write(`Configured memory model fields: ${settingsPath}\n`);
    if (useAPI) {
      process.stdout.write("Fill memory_bge_api_key/base_url/model and optional reranker fields before using memory.\n");
    }
    break;
  default:
    fail(`unknown command ${args.command}`);
}
