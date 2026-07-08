# LuminaCode

[简体中文](README.zh-CN.md)

LuminaCode is a terminal-first coding agent with a Go backend runtime and a
TypeScript terminal frontend. It works inside the current project directory,
builds context from local files and project instructions, uses tools with
permission checks, and keeps sessions resumable across long-running development
work.

The goal is not only to chat with a model, but to give the model an agent
runtime: project context, skills, tools, memory, MCP integrations, session
state, and safety boundaries.

## Agent Capabilities

- Understands the current project directory and uses it as the default working
  root.
- Reads project instructions from `LUMINA.md` or `AGENTS.md`, with a fallback to
  user-level instructions under `~/.lumina`.
- Loads reusable skills from project, user, legacy project, and bundled skill
  directories.
- Injects selected skill context into model requests without polluting the
  visible dialogue.
- Executes file and shell tools with permission prompts and safety checks.
- Supports MCP servers declared by the project and stores trusted server
  fingerprints under the project root.
- Maintains session state, message history, tool results, and recovery data so
  sessions can be resumed.
- Stores each session in its own session directory, including transcript,
  state, task, skill-recovery, and session-memory data.
- Tracks context-window usage and uses the configured context limit for local
  compression decisions.
- Supports OpenAI-compatible and Anthropic-compatible streaming APIs.
- Preserves provider error details, including status codes and raw response
  bodies, for easier debugging.
- Separates user-visible conversation from internal reasoning, tool calls, tool
  results, and other runtime records.
- Supports sub-agents with explicit per-call timeouts and graceful timeout
  finalization.
- Provides headless execution and benchmark harness modes without requiring the
  interactive frontend.

## Architecture

The installed command is split into two processes:

- `lumina`: the TypeScript terminal frontend and default user entry point.
- `lumina-backend`: the Go runtime that owns agent execution, tools, skills,
  MCP, memory, sessions, and headless/benchmark modes.

Interactive sessions communicate with the backend over a localhost WebSocket.
The backend endpoint is written to:

```text
~/.lumina/run/backend.json
```

The backend only listens on `127.0.0.1` and WebSocket connections must carry the
generated auth token. Multiple sessions can exist at the same time; each session
has independent runtime state, and only one active submit is allowed within a
single session.

Single-shot prompts, session listing, and benchmark/headless paths are forwarded
to `lumina-backend` and do not depend on the interactive frontend.

## Quick Start

Install the CLI:

```sh
# macOS/Linux
make install
```

On Windows PowerShell:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\install-windows.ps1
```

Start LuminaCode from any project directory:

```sh
cd /path/to/project
lumina
```

Run a single prompt and exit:

```sh
lumina -p "Summarize this repository"
```

List saved sessions:

```sh
lumina --list
```

Resume a saved session:

```sh
lumina --resume <session-id>
```

By default, the working directory is the directory where `lumina` is started.
You usually do not need to pass `--cwd`.

## API Configuration

LuminaCode does not hard-code a default model. Configure the provider through
environment variables, command-line flags, or installed defaults.

Example environment variables:

```sh
export LUMINA_API_KEY="..."
export LUMINA_API_BASE_URL="https://api.deepseek.com/anthropic"
export LUMINA_API_MODEL="deepseek-v4-pro[1m]"
export LUMINA_API_TYPE="anthropic"
```

LuminaCode also accepts the generic `LLM_API_KEY`, `LLM_BASE_URL`,
`LLM_DEFAULT_MODEL`, and `LLM_API_TYPE` environment variables. If both sets are
present, `LUMINA_*` takes precedence.

Equivalent runtime flags:

```sh
lumina \
  --api-key "$LUMINA_API_KEY" \
  --base-url "https://api.deepseek.com/anthropic" \
  --api-type anthropic \
  --model "deepseek-v4-pro[1m]" \
  --max-tokens 1000000
```

Supported API type values:

- `anthropic`
- `openai_compatible`
- `auto`

`--max-tokens` represents the model context-window size used by LuminaCode for
local accounting and compression thresholds. The local compression threshold is
`80%` of that value.

LuminaCode does not force a provider-side completion `max_tokens` parameter into
API requests.

The installed defaults file is:

```text
~/.lumina/CONFIG/defaults.json
```

LuminaCode reloads runtime configuration before each agent turn, so edits to
this file can affect new requests without reinstalling.

## Project Instructions

LuminaCode first checks the current working directory for:

1. `LUMINA.md`
2. `AGENTS.md`

If neither file exists, it falls back to:

1. `~/.lumina/LUMINA.md`
2. `~/.lumina/AGENTS.md`

These files are optional. Use them to document repository conventions, testing
commands, coding style, tool restrictions, or domain-specific guidance.

## Skills

Skills are reusable instruction packages stored in `SKILL.md` files. LuminaCode
loads them from:

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`
- `~/.lumina/skills/`
- `~/.lumina/SKILLS/`

A skill can provide focused behavior such as code review, repository analysis,
paper writing, experiment execution, or project-specific workflows. When a skill
is invoked, its processed prompt is injected into the model context for that
turn while remaining separate from the visible chat transcript.

Example invocation:

```text
/review inspect the authentication flow
```

## Tools and Permissions

LuminaCode gives the model access to local capabilities through tools. Built-in
tools cover common coding-agent needs such as reading files, editing files,
running shell commands, managing tasks, and interacting with memory.

Tool execution is guarded by permission logic. Risky operations can require
human approval before they run. Project MCP servers also require trust approval
before their tools are exposed.

## MCP

Project MCP servers can be configured with `.mcp.json`. LuminaCode prompts
before trusting project MCP servers and stores accepted fingerprints in:

```text
~/.lumina/project/{project_root_name}/CONFIG/trusted_mcp.json
```

Use `/mcp` during an interactive session to inspect registered MCP tools.

## Sessions and Runtime Data

LuminaCode keeps enough runtime state to resume previous sessions, including
messages, tool results, task recovery data, and skill recovery metadata.

Installed resources live under:

```text
~/.lumina/
```

Common installed resources:

- `CONFIG/defaults.json`
- `SYSTEM/system-prompt.md`
- `SYSTEM/extraction_system.md`
- `SKILLS/`
- `frontend/`

Project-scoped runtime data generated by LuminaCode lives under:

```text
~/.lumina/project/{project_root_name}/
```

Common project-scoped runtime data:

- `CONFIG/trusted_mcp.json`
- `agent-memory/`
- `agent-memory-local/`
- `background/tool-results/`

Project-authored resources can still live in the project, for example:

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`

Session history lives under the configured `session_dir`. The built-in default is
`~/.lumina/sessions`; installed configuration can override it through
`~/.lumina/CONFIG/defaults.json`. Each session is stored in its own subdirectory
named by session id:

```text
{session_dir}/{session_id}/
```

Common per-session files:

- `transcript.jsonl`
- `transcript.md`
- `meta.json`
- `state.json`
- `tasks.json`
- `skill-recovery.json`
- `skill-recovery.commit.json`
- `session.sqlite`

Large background tool outputs are stored under:

```text
~/.lumina/project/{project_root_name}/background/tool-results/
```

## CLI Reference

```text
lumina [flags]
```

`lumina` starts the TypeScript frontend for interactive sessions. The Go
backend can also be called directly:

```sh
lumina-backend -p "Summarize this repository"
lumina-backend --list
lumina-backend daemon --host 127.0.0.1 --port 0
```

Common flags:

- `-p`, `-prompt`: run a single prompt and exit
- `-resume`: resume a previous session by ID
- `-list`: list saved sessions
- `-cwd`: explicitly set the working directory
- `-model`: model name
- `-api-key`: API key
- `-base-url`: API base URL
- `-api-type`: `openai_compatible`, `anthropic`, or `auto`
- `-max-tokens`: context-window token limit used for local accounting
- `-yolo`: skip permission prompts
- `-bare`: disable auto-memory and other persistent features
- `-verbose`, `-v`: enable debug output

The old Go interactive TUI has been removed. Interactive use goes through the
TypeScript `lumina` frontend; headless use goes through `lumina` passthrough or
`lumina-backend`.

## Installation

Install on macOS/Linux:

```sh
make install
```

Install on Windows PowerShell:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\install-windows.ps1
```

The Windows installer builds `lumina-backend.exe`, builds the TypeScript
frontend, installs a `lumina.cmd` launcher, copies bundled resources to
`%USERPROFILE%\.lumina`, and adds the install directory to the user `PATH`
unless `-NoPathUpdate` is passed.

Inspect detected paths and shell setup on macOS/Linux:

```sh
make doctor
```

Inspect the Windows install:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\doctor-windows.ps1
```

Uninstall on macOS/Linux:

```sh
make uninstall
```

Uninstall on Windows:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\uninstall-windows.ps1
```

`make uninstall` removes the installed `lumina` and `lumina-backend` commands
and the `~/.lumina` resource directory. It does not edit shell rc files and does
not remove project-local `.Lumina` data.

## Development

Run tests:

```sh
go test ./...
npm --prefix frontend test
```

Build:

```sh
# macOS/Linux
make build
```

Install locally:

```sh
# macOS/Linux
make install
```

Build and run from the source tree on Windows:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\setup-windows.ps1
.\tmp\lumina.cmd --help
```

## Repository Layout

- `agent/`: agent loop, state, permissions, memory injection, and tool execution
- `agentContext/`: context compression and injection pipeline
- `api/`: streaming LLM clients and provider protocol normalization
- `backend/`: WebSocket daemon, session manager, and frontend IPC bridge
- `cli/`: slash command classification and completion helpers
- `config/`: configuration loading, environment overrides, and path resolution
- `frontend/`: TypeScript terminal frontend
- `mcp/`: MCP config, trust, and dynamic tool registration
- `memory/`: auto-memory storage and recall
- `security/`: command and path safety checks
- `session/`: session persistence, migration, and recovery
- `sessionmemory/`: per-session memory commit log and history tools
- `skills/`: skill loading, prompt processing, discovery, and execution
- `tools/`: built-in tools
- `ui/`: shared runtime frame model and legacy renderer tests
- `test/`: parity and regression tests
