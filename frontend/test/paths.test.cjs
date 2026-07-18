const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const { appRoot } = require("../dist/paths.js");
const { shouldPassthrough } = require("../dist/backend.js");

test("shared AppRoot path contract", () => {
  const fixture = fs.readFileSync(path.join(__dirname, "..", "..", "testdata", "app-path-contract.tsv"), "utf8");
  for (const line of fixture.split(/\r?\n/)) {
    if (!line || line.startsWith("#")) continue;
    const [name, platform, rawHome, rawLocal, rawOverride, expected] = line.split("|");
    const home = rawHome === "-" ? "" : rawHome;
    const local = rawLocal === "-" ? "" : rawLocal;
    const override = rawOverride === "-" ? "" : rawOverride;
    const env = { LOCALAPPDATA: local, LUMINA_APP_ROOT: override };
    const nodePlatform = platform === "windows" ? "win32" : platform;
    assert.equal(appRoot(nodePlatform, env, home), expected, name);
  }
});

test("darwin and linux use the single home AppRoot", () => {
  for (const platform of ["darwin", "linux"]) {
    assert.equal(appRoot(platform, {}, "/Users/tester"), path.join("/Users/tester", ".lumina"));
  }
});

test("windows prefers LOCALAPPDATA and falls back to the user home", () => {
  assert.equal(appRoot("win32", { LOCALAPPDATA: "C:\\Local" }, "C:\\Users\\tester"), "C:\\Local\\LuminaCode");
  assert.equal(appRoot("win32", {}, "C:\\Users\\tester"), "C:\\Users\\tester\\.lumina");
});

test("LUMINA_APP_ROOT is the only AppRoot override", () => {
  const root = "/tmp/lumina custom root";
  assert.equal(appRoot("linux", { LUMINA_APP_ROOT: root, LOCALAPPDATA: "/ignored" }, "/home"), path.posix.normalize(root));
  assert.throws(() => appRoot("linux", { LUMINA_APP_ROOT: "relative" }, "/home"), /must be absolute/);
});

test("root targets and missing homes are rejected", () => {
  assert.throws(() => appRoot("linux", { LUMINA_APP_ROOT: "/" }, "/home"), /unsafe/);
  assert.throws(() => appRoot("linux", {}, ""), /home is empty/);
  assert.throws(() => appRoot("linux", {}, "relative-home"), /must be absolute/);
  assert.throws(() => appRoot("win32", { LOCALAPPDATA: "relative-local" }, "C:\\Users\\tester"), /must be absolute/);
});

test("layout management commands are passed directly to the Go CLI", () => {
  for (const command of ["layout", "memory", "daemon", "shutdown"]) {
    assert.equal(shouldPassthrough([command, "doctor"]), true);
  }
});
