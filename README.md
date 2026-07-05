# LuminaCode

[简体中文](README.zh-CN.md)

LuminaCode is a Go implementation of a terminal-first coding agent. It works
inside the current project directory, builds context from local files and
project instructions, uses tools with permission checks, and keeps sessions
resumable across long-running development work.

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
- Tracks context-window usage and uses the configured context limit for local
  compression decisions.
- Supports OpenAI-compatible and Anthropic-compatible streaming APIs.
- Preserves provider error details, including status codes and raw response
  bodies, for easier debugging.
- Separates user-visible conversation from internal reasoning, tool calls, tool
  results, and other runtime records.

## Quick Start

Install the CLI:

```sh
make install
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
- `~/.Lumina/skills/`
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
{project_root}/.Lumina/CONFIG/trusted_mcp.json
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

Project-local runtime data lives under:

```text
{project_root}/.Lumina/
```

Common project-local data:

- `.Lumina/worktrees/`
- `.Lumina/CONFIG/trusted_mcp.json`
- `.Lumina/PROJECT_SKILLS/`

Session history lives under:

```text
~/.Lumina/sessions/
```

## CLI Reference

```text
lumina [flags]
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

The fullscreen interface is the only supported interactive mode.

## Installation

Install:

```sh
make install
```

Inspect detected paths and shell setup:

```sh
make doctor
```

Uninstall:

```sh
make uninstall
```

`make uninstall` removes the installed binary and `~/.lumina` resources. It does
not edit shell rc files and does not remove project-local `.Lumina` data.

## Development

Run tests:

```sh
go test ./...
```

Build:

```sh
go build ./...
```

Install locally:

```sh
make install
```

## Repository Layout

- `agent/`: agent loop, state, permissions, memory injection, and tool execution
- `agentContext/`: context compression and injection pipeline
- `api/`: streaming LLM clients and provider protocol normalization
- `cli/`: slash command classification and completion helpers
- `config/`: configuration loading, environment overrides, and path resolution
- `mcp/`: MCP config, trust, and dynamic tool registration
- `memory/`: auto-memory storage and recall
- `security/`: command and path safety checks
- `session/`: session persistence and recovery
- `skills/`: skill loading, prompt processing, discovery, and execution
- `tools/`: built-in tools
- `ui/`: interactive runtime frame model and terminal rendering
- `test/`: parity and regression tests
