# LuminaCode AppRoot Layout

AppRoot is the shared storage contract used by the Go backend, TypeScript
frontend, POSIX installers, and PowerShell installers. Project-local `.Lumina`
directories are unchanged.

## Root Resolution

| Platform | Default |
|---|---|
| macOS | `$HOME/.lumina` |
| Linux | `$HOME/.lumina` |
| Windows | `%LOCALAPPDATA%\LuminaCode` |
| Windows fallback | `%USERPROFILE%\.lumina` |

`LUMINA_APP_ROOT` is the only AppRoot override and must be absolute.
`LUMINA_RESOURCE_ROOT` overrides bundled resources without changing data paths.
`LUMINA_HOME` is a deprecated resource-only alias and emits a warning.

## Logical Layout

```text
<AppRoot>/
├── layout.json
├── app/                         # installer-owned, replaceable
│   ├── frontend/
│   ├── resources/{defaults,system,skills,teams}/
│   ├── extensions/arxiv-mcp/
│   └── scripts/
├── config/                      # user-owned, back up
│   ├── settings.json
│   ├── mcp.json
│   ├── instructions/{LUMINA.md,AGENTS.md}
│   ├── prompts/
│   ├── skills/
│   └── teams/
├── data/                        # durable, back up
│   ├── memory/fabric/{ledger.sqlite,index.sqlite}
│   ├── sessions/{active,archive}/
│   ├── projects/<project-id>/{project.json,trust/mcp.json,teams/}
│   └── legacy/layout/
├── state/                       # machine-local runtime state
│   ├── run/backend.json
│   ├── logs/backend.log
│   ├── managed/mcp.json
│   ├── services/searxng/
│   ├── migrations/
│   └── projects/<project-id>/tool-results/<session-id>/
└── cache/                       # disposable and rebuildable
    ├── models/memory/
    ├── downloads/
    └── tmp/
```

`layout.json` records `layout_version`, `installed_version`, and `platform`.
The runtime refuses writes when the file is missing, invalid, or has an
unsupported revision. Fresh installation and legacy conversion initialize it only
through `layout migrate --apply`.

Project IDs use `<slug>-<hash>`, where the slug is derived from the project
basename and the 16-character hash is derived from its canonical absolute root.
Canonicalization resolves symlinks when possible; Windows additionally
normalizes drive letters, UNC paths, separators, and case.

Resource lookup order is project-local resources, user `config` overrides,
then bundled `app/resources`. Configuration lookup order is compiled defaults,
user settings, project defaults, environment variables, then CLI flags.

## Commands

```sh
lumina layout paths --json
lumina layout doctor --json
lumina layout migrate --dry-run
lumina layout migrate --apply
lumina layout bind-project --legacy <name> --root <path>
```

Dry-run performs discovery, byte accounting, project mapping, resource
comparison, and conflict detection without writing. Apply stops the daemon,
uses a migration lock, verifies copied content, discards retired memory data,
and writes `state/migrations/migration-report.json`. `layout.json` is committed only
after pre-commit migration checks succeed.

Existing destinations are skipped only when content is identical. Different
databases, settings, trust records, sessions, or other destinations stop the
migration; they are never silently overwritten. Ambiguous projects are moved
to `data/legacy/layout/projects` and their trust remains inactive until explicitly
bound.

## Install And Removal

Installers build `app.new`, migrate and validate the layout, then rename the
current `app` to `app.old` and switch the staged app into place. A failed app
health check restores the prior app. Frontend payloads contain only compiled
output and production dependencies. POSIX automation can set
`NO_PATH_UPDATE=1`; Windows automation can pass `-NoPathUpdate` to prevent
shell or user PATH changes.

Normal uninstall removes commands plus `app`, `cache`, and `state`, while
preserving `layout.json`, `config`, and `data`. Use `make purge`,
`make uninstall PURGE=1`, or `uninstall-windows.ps1 -Purge` for permanent
AppRoot deletion.

On POSIX systems, private layers use mode `0700` and sensitive files use
`0600`. On Windows, installers disable broad ACL inheritance for
`config/data/state` and grant access to the current user and SYSTEM.
