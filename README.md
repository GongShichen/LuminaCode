# LuminaCode

[简体中文](README.zh-CN.md)

LuminaCode is a local general-purpose Agent. It uses a Go backend and a
TypeScript TUI, with project context, tools, skills, MCP, memory, sessions, and
Agent Teams.

## Agent Capabilities

- Uses the launch directory as the default project root.
- Reads `LUMINA.md` / `AGENTS.md`, with user-level fallback under
  `<AppRoot>/config/instructions`.
- Loads project, user, and bundled skills.
- Runs file, shell, web, MCP, memory, and task tools with permission controls.
- Keeps resumable sessions with transcript, state, tasks, tool results, skill
  recovery, and session memory.
- Supports OpenAI-compatible and Anthropic-compatible streaming APIs.
- Keeps visible chat separate from tool payloads, tool results, and runtime
  records.
- Supports sub-agents, Agent Teams, headless mode, and benchmark harnesses.

## Architecture

The installed command is split into two processes:

- `lumina`: the TypeScript terminal frontend and default user entry point.
- `lumina-backend`: the Go runtime that owns agent execution, tools, skills,
  MCP, memory, sessions, and headless/benchmark modes.

Interactive sessions use localhost WebSocket:

```text
<AppRoot>/state/run/backend.json
```

The backend listens on `127.0.0.1`, requires an auth token, supports multiple
sessions, and serializes submits within each session. Headless paths are handled
by `lumina-backend`.

## Long-Term Memory

Memory Fabric keeps durable cross-session state in two local SQLite databases
and builds a third, replaceable BGE-M3 retrieval index:

```text
<AppRoot>/data/memory/fabric/ledger.sqlite
<AppRoot>/data/memory/fabric/index.sqlite
<AppRoot>/data/memory/fabric/retrieval-bge-m3.sqlite
```

### Memory Write Flow

1. **Durable evidence ingest** commits visible user, assistant, and tool events
   as immutable, source-addressable ledger rows before any semantic work.
2. **Context and provenance binding** records the project space, context,
   session, actor, timestamp, turn order, checksum, and source offsets needed
   to reconstruct each event.
3. **Semantic compilation** optionally uses the configured API model to select
   durable memories and produce grounded nodes, identities, slots, temporal
   scope, retrieval cues, and conflict candidates. Every node must cite source
   events and pass local grounding validation.
4. **Conflict resolution** applies local authority and time rules to clear
   updates. Ambiguous cases remain pending or use the configured adjudicator;
   raw evidence is never overwritten.
5. **Local BGE-M3 indexing** creates dense vectors and the event/span dense,
   learned-sparse, FTS, and graph representations used by retrieval.
6. **Recoverable publication** checkpoints background jobs and atomically
   publishes derived indexes. Model, tokenizer, or schema changes rebuild
   derived data from the ledger without re-ingesting the conversation.

### Memory Retrieval Flow

Retrieval is fully local and runs the same pipeline for every ordinary
natural-language query:

1. **Alignment** verifies the retrieval index model, tokenizer, schema, and
   ledger checksum, then incrementally catches up missing events when needed.
2. **Query encoding** produces BGE-M3 dense, learned-sparse, and full-token
   representations.
3. **Candidate recall** runs span FTS5, event dense, and learned-sparse
   channels, each with up to 128 candidates.
4. **Fusion and graph expansion** use reciprocal-rank fusion for the recall
   pool and Personalized PageRank over event, context, semantic, and
   sparse-concept links.
5. **Exact span scoring** applies BGE-M3 dense, learned-sparse, and full-token
   ColBERT MaxSim with the fixed `1.0 : 0.3 : 1.0` fusion.
6. **Evidence selection** deduplicates source events and uses a submodular
   objective to maximize relevance and coverage under the configured item and
   token budgets.
7. **Context assembly** applies reference-time and conflict-state filters,
   groups evidence coherently, and preserves timestamps, actors, contexts,
   source IDs, and span positions for the answer model.

Identifiers, paths, and quoted text are lexical candidate features only; query
content never routes around BGE-M3. Search has no remote query expansion,
question-type routing, benchmark-specific entities, or local answer bypass.

### LongMemEval-S

On the complete 500-question LongMemEval-S full-haystack evaluation,
LuminaCode's Memory Fabric, using local BGE-M3 retrieval, scored
**83.00% (415/500)** with `mimo-v2.5-pro` as both answer model and official
evaluator.

| Question type | Correct | Accuracy |
|---|---:|---:|
| Knowledge update | 74/78 | 94.87% |
| Single-session user | 65/70 | 92.86% |
| Temporal reasoning | 114/133 | 85.71% |
| Multi-session | 103/133 | 77.44% |
| Single-session assistant | 40/56 | 71.43% |
| Single-session preference | 19/30 | 63.33% |

End-to-end retrieval averaged **1.02 seconds** (P50 **0.99 seconds**,
P95 **1.29 seconds**).

Published LongMemEval accuracy, sorted for orientation:

| System | Accuracy | Reported evaluation |
|---|---:|---|
| Exabase M-1 | 96.4% | Gemini 3 Flash, Top 50; vendor-reported |
| Mastra Observational Memory | 94.87% | GPT-5-mini; open implementation and runner |
| Mem0 Platform | 94.8% | Mem0 current benchmark, Top 50 |
| Honcho | 92.6% | Publicly reported; full run configuration not disclosed |
| Engram | 91.6% | GPT-5 composer, GPT-4o judge; prompt and run artifacts published |
| Hindsight | 91.4% | Gemini 3 Pro; benchmark repository published |
| HydraDB | 90.79% | Gemini 3 Pro; paper-reported |
| **LuminaCode (LongMemEval-S)** | **83.0%** | Full haystack; `mimo-v2.5-pro` answer and official evaluator; 500 questions |
| LiCoMemory | 73.8% | GPT-4o-mini, five-run mean |
| Mem0-G | 64.8% | GPT-4o-mini controlled baseline |
| Mem0 | 62.6% | GPT-4o-mini controlled baseline |
| Zep | 58.6% | GPT-4o-mini controlled baseline |
| A-Mem | 55.0% | GPT-4o-mini controlled baseline |
| MemOS | 51.2% | GPT-4o-mini controlled baseline |

Sources: [LongMemEval](https://github.com/xiaowu0162/longmemeval),
[Mem0 benchmark](https://github.com/mem0ai/memory-benchmarks),
[LiCoMemory paper](https://aclanthology.org/2026.findings-acl.1835/),
[Mastra Observational Memory](https://mastra.ai/research/observational-memory),
[Hindsight benchmarks](https://github.com/vectorize-io/hindsight-benchmarks),
[Engram benchmark](https://lumetra.io/engram-on-longmemeval/),
[HydraDB paper](https://research.hydradb.com/hydradb.pdf),
[Honcho](https://github.com/plastic-labs/honcho), and the
[Exabase M-1 announcement](https://www.prnewswire.com/news-releases/exabase-achieves-highest-reported-score-on-leading-ai-memory-benchmark-using-a-smaller-cheaper-model-302780919.html).
Exabase and Honcho scores are included as publicly reported results with less
complete reproduction material. Reader, retrieval depth, context budget,
answer model, and judge differ across reports, so this table is not a strict
apples-to-apples leaderboard.

## Agent Team

Agent Team mode runs a task through isolated specialist agents. Each member has
its own prompt, skills, context, task state, and A2A inbox/outbox while sharing
the same backend runtime and tool system.

Commands:

```text
/Team      choose an installed team and enter Team mode
/TeamOut   leave Team mode
/NewTeam   create a new editable team template
```

The TUI shows Team dialogue as a group chat. Raw tool payloads, full tool
results, MCP payloads, and hidden reasoning stay in runtime logs.

Runtime summary:

- Loop: observe -> plan -> dispatch -> agent work -> collect -> gate -> finalize.
- Stop policy: user interrupt or task complete.
- Failures become recovery inputs for the next loop.
- Ordinary Agent context and Team Agent contexts remain isolated.

```text
<AppRoot>/data/projects/{project-id}/teams/{team_name}/{team_session_id}/
<AppRoot>/data/projects/{project-id}/teams/{team_name}/{team_session_id}/agents/{agent_id}/
```

### Built-in Teams

Installed under `<AppRoot>/app/resources/teams/`:

- `product-development`: full-stack delivery with `team-leader`, `research`,
  `frontend`, `backend`, `qa`, `reviewer`, `devops`, and `ux-design`. Uses
  contract, QA, reviewer, task-policy, and follow-up/deferral gates.
- `deep-research`: research team with `team-leader`, `scope-planner`,
  `search-strategist`, `source-reader`, `evidence-analyst`, `report-writer`,
  `qa`, and `reviewer`. Uses SearxNG `WebSearch` / `WebFetch` and arXiv MCP;
  can export report and evidence files.

Team lookup order is project `.Lumina/TEAM`, user `<AppRoot>/config/teams`, then
installed `<AppRoot>/app/resources/teams`.

### Creating a Team

`/NewTeam` asks for a display name and creates:

```text
<AppRoot>/config/teams/{team_name}/
├── team.yaml
├── team-system.md
├── shared-prompt.md
├── completion-policy.md
└── team-leader/
    ├── agent.yaml
    ├── system.md
    └── skills/
```

The template starts with only `team-leader`. Add new agent directories and list
their ids in `team.yaml`.

### Team Configuration

Minimal `team.yaml` shape:

```yaml
name: my-team
display_name: My Team
entry_agent: team-leader
loop:
  max_iterations: 0
  max_parallel_agents: 2
  completion_policy: team_leader_only
  stop_policy: user_interrupt_or_task_complete_only
gates:
  require_contract: false
  checks: []
transcript:
  show_member_dialogue: true
  show_tool_details: false
  show_thinking: false
agents:
  - team-leader
```

Agent `agent.yaml`:

```yaml
name: team-leader
display_name: Team Leader
communicates_with: all
model: inherit
tools: inherit
max_turns_per_task: 0
private_skills: true
```

`communicates_with` can be `all` or a list of agent ids. Private skills live in
that agent's `skills/` directory.

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

Single prompt:

```sh
lumina -p "Summarize this repository"
```

List sessions:

```sh
lumina --list
```

Resume:

```sh
lumina --resume <session-id>
```

The launch directory is the default working directory.

## API Configuration

No default model is hard-coded. Configure via env, flags, or
`<AppRoot>/config/settings.json`:

```sh
export LUMINA_API_KEY="..."
export LUMINA_API_BASE_URL="https://api.deepseek.com/anthropic"
export LUMINA_API_MODEL="deepseek-v4-pro[1m]"
export LUMINA_API_TYPE="anthropic"
```

Generic `LLM_*` variables are also accepted; `LUMINA_*` wins. Equivalent flags:

```sh
lumina \
  --api-key "$LUMINA_API_KEY" \
  --base-url "https://api.deepseek.com/anthropic" \
  --api-type anthropic \
  --model "deepseek-v4-pro[1m]" \
  --max-tokens 1000000
```

Configuration precedence is: compiled defaults, user
`config/settings.json`, project `.Lumina/CONFIG/defaults.json`, environment,
then CLI flags. Default paths are derived by `apppaths` and are not written to
user settings.

`--api-type`: `anthropic`, `openai_compatible`, or `auto`.

An optional global fallback can use a different endpoint and model:

```json
{
  "fallback_api_enabled": true,
  "fallback_api_key": "...",
  "fallback_api_base_url": "https://api.example.com/anthropic",
  "fallback_api_model": "fallback-model",
  "fallback_api_type": "anthropic"
}
```

After the primary client's retries are exhausted, Lumina switches only for
429, 5xx, timeout, EOF, and transport failures. It does not hide invalid keys,
invalid requests, or model configuration errors, and never switches after the
primary stream has produced visible output or a tool call. Config changes are
hot-read at the next turn.

`--max-tokens` is the local context-window size used for accounting and the 80%
compression threshold. LuminaCode does not force provider-side completion
`max_tokens`. Runtime config is hot-read before each agent turn.

## Project Instructions

Read order:

1. `{cwd}/LUMINA.md`
2. `{cwd}/AGENTS.md`
3. `<AppRoot>/config/instructions/LUMINA.md`
4. `<AppRoot>/config/instructions/AGENTS.md`

All files are optional.

## Skills

Skills are `SKILL.md` instruction packages loaded from:

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`
- `<AppRoot>/config/skills/`
- `<AppRoot>/app/resources/skills/`

Invoked skill context is injected into the model request without entering the
visible transcript.

```text
/review inspect the authentication flow
```

## Tools and Permissions

Tools cover file edits, shell, tasks, memory, web search/fetch, and MCP. Risky
operations and project MCP servers can require approval.

## MCP

Project MCP config: `.mcp.json`. Trust records:

```text
<AppRoot>/data/projects/{project-id}/trust/mcp.json
```

Use `/mcp` to inspect registered MCP tools.

## Sessions and Runtime Data

AppRoot uses five ownership layers:

```text
<AppRoot>/
```

- `app/`: atomically replaceable application payload and bundled resources
- `config/`: user settings, MCP config, instructions, skills, and teams
- `data/`: memory, sessions, project manifests/trust/team data, and legacy data
- `state/`: endpoint, logs, services, migrations, and per-session tool results
- `cache/`: models, downloads, and temporary rebuildable files

Project runtime data:

```text
<AppRoot>/data/projects/{project-id}/
```

- `project.json`
- `trust/mcp.json`
- `teams/`

Project-authored resources:

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`

Active session history uses `<AppRoot>/data/sessions/active` by default; archived
sessions use `<AppRoot>/data/sessions/archive`:

```text
{session_dir}/{session_id}/
```

- `transcript.jsonl`
- `transcript.md`
- `meta.json`
- `state.json`
- `tasks.json`
- `skill-recovery.json`
- `skill-recovery.commit.json`
- `session.sqlite`

Large background outputs:

```text
<AppRoot>/state/projects/{project-id}/tool-results/{session-id}/
```

## CLI Reference

```text
lumina [flags]
```

`lumina` starts the TS frontend. Direct backend usage:

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
- `-yolo`: skip permission prompts and OS sandbox isolation
- `-bare`: disable auto-memory and other persistent features
- `-verbose`, `-v`: enable debug output

Interactive mode uses the TypeScript frontend; headless mode uses `lumina`
passthrough or `lumina-backend`.

## Installation

Default AppRoot resolution is `$HOME/.lumina` on macOS/Linux and
`%LOCALAPPDATA%\LuminaCode` on Windows, falling back to
`%USERPROFILE%\.lumina`. `LUMINA_APP_ROOT` is the only root override;
`LUMINA_RESOURCE_ROOT` changes bundled resources only. See
[AppRoot layout](docs/app-root.md) for the complete storage and migration
contract.

macOS/Linux:

```sh
make install
```

The default install first checks the host hardware, required toolchain, free
space, and usable execution provider. It then downloads a revision- and
SHA-256-pinned BGE-M3 profile from ModelScope: MLX INT8 with the managed Metal
runtime on Apple Silicon, ONNX INT8 for CPU, or ONNX FP16 for a supported
managed accelerator runtime. It replaces the installed application only after
the model, tokenizer, linear heads, native runtime, and inference probe pass.
BGE-M3 is the sole local model for memory writes and retrieval; installation
fails without a valid model and does not fall back to another embedding space.
`LUMINA_MEMORY_EMBEDDING_DEVICE` selects a device explicitly, while
`LUMINA_MEMORY_MODEL_VARIANT=metal-int8|cpu-int8|accelerator-fp16` pins a
packaging profile.

Installation output is streamed to the terminal and an install log. If any
stage fails, the installer exits nonzero and reports the failed stage, original
error message, exit code, log path, and rollback state. An incomplete
`app.new` is removed and an application swap is restored automatically.

For unattended installs that must not edit a shell profile, use
`make install NO_PATH_UPDATE=1`. The Windows equivalent is `-NoPathUpdate`.

Windows:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\install-windows.ps1
```

Doctor:

```sh
make doctor
lumina layout paths --json
lumina layout doctor --json
```

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\doctor-windows.ps1
```

Uninstall:

```sh
make uninstall
```

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\uninstall-windows.ps1
```

`make uninstall` shuts down backend/MCP/SearxNG, removes installed commands and
the installer-owned `app/cache/state` layers. It preserves `config`, `data`,
`layout.json`, shell rc files, and project-local `.Lumina`. Permanent removal is
explicit:

```sh
make purge
# or: make uninstall PURGE=1
```

```powershell
.\scripts\uninstall-windows.ps1 -Purge
```

## Development

Test:

```sh
go test ./...
npm --prefix frontend test
```

Build:

```sh
# macOS/Linux
make build
```

Install:

```sh
# macOS/Linux
make install
```

Windows source-tree setup:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\setup-windows.ps1
.\tmp\lumina.cmd --help
```

## Repository Layout

- `agent/`: agent loop, state, permissions, memory injection, and tool execution
- `agentContext/`: context compression and injection pipeline
- `apppaths/`: cross-platform AppRoot, project identity, doctor, and migration
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
- `team/`: Agent Team configuration, runtime loop, A2A dialogue, gates, and
  persistence
- `tools/`: built-in tools
- `ui/`: shared runtime frame model and legacy renderer tests
- `test/`: parity and regression tests
