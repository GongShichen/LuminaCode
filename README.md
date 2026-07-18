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

Cross-session memory lives in one local SQLite store:

```text
<AppRoot>/data/memory/lumina-memory.sqlite
```

### 1. What It Remembers And How

- Lumina stores the visible conversation, useful tool observations, user
  preferences, project decisions, reusable procedures, facts, entities, events,
  and their relationships. Hidden reasoning, credentials, permission payloads,
  and duplicated tool dumps are excluded.
- Original messages are committed first as immutable evidence. They are split
  into larger chunks and small sentence/list/code-aware evidence atoms, while
  retaining the Session, role, timestamp, source offsets, provenance, and access
  scope needed to reconstruct the original context.
- Recoverable background jobs enrich that evidence into structured facts,
  entities, relations, events, preferences, and local E5 embeddings. Updates do
  not erase history: factual validity, observation time, conflicts, and
  superseded versions remain traceable.
- User, project, Team, agent-type, and Team-agent memories remain isolated by
  scope. Frequently used or reinforced memories stay hot; low-value expired
  records may be archived, but background maintenance never physically deletes
  them.

`make install` downloads `multilingual-e5-small` from ModelScope into
`<AppRoot>/cache/models/memory/`; normal uninstall removes this rebuildable
cache. `/Memory`,
`/MemorySearch`, `/MemoryForget`, `/MemoryExport`, and `/MemoryImport` provide
local inspection and control.

### 2. What It Finds, How It Finds It, And Benchmark Results

- For every query, Lumina searches exact wording, semantic similarity, entities,
  time, Sessions, and relations through fixed BM25, vector, entity, temporal,
  Session, and graph channels. Generic model-generated query expansion may add
  synonyms or structured hints, but cannot disable a channel, change scope, or
  exclude a memory type.
- Independent signals are fused once, then the Evidence Ledger selects small,
  source-linked atoms that cover the query's distinct information needs. If
  support is incomplete, one residual all-channel search is performed. The main
  model receives the selected evidence, local structural context, provenance,
  and timeline as one transient hidden packet rather than a repeated Session
  summary or full transcript.

On the 500-question LongMemEval oracle set, Lumina scored **86.0% (430/500)**
using the official LongMemEval judge prompt with `deepseek-v4-pro` through
`https://api.deepseek.com`. This is a black-box test of the production memory
path, not an official GPT-4o leaderboard score.

| Question type | Accuracy |
|---|---:|
| Single-session user | 97.14% |
| Knowledge update | 91.03% |
| Temporal reasoning | 88.72% |
| Single-session preference | 80.00% |
| Multi-session | 79.70% |
| Single-session assistant | 76.79% |

The run also records retrieval quality separately from answer accuracy:

| Retrieval metric | Result |
|---|---:|
| Evidence hit rate | 99.79% |
| Evidence Recall@K | 95.75% |
| Evidence MRR | 0.701 |
| Source Session recall | 100.00% |
| Gold message recall | 98.05% |
| Injected chunk recall | 95.75% |
| Injected text coverage | 88.13% |
| Average memory context | 1,717 tokens (22.59% memory token ratio) |
| Average retrieval duration | 8.34 seconds |

Retrieval metrics are computed from the evidence atom IDs and source spans
actually injected into the answering model.

#### Full-haystack LongMemEval-S

The oracle result above is an upper-bound retrieval setting: each question is
given only the answer-supporting Sessions. The cleaned LongMemEval-S set uses
the same 500 question IDs but supplies the complete conversation haystack. This
increases the average search space from 1.90 to 47.73 Sessions and from 21.92 to
493.50 messages per question.

Using the same `mimo-v2.5-pro` answer model and `deepseek-v4-pro` official judge
prompt, Lumina scored **75.8% (379/500)** on LongMemEval-S:

| Question type | Oracle | LongMemEval-S | Difference |
|---|---:|---:|---:|
| Overall | 86.00% (430/500) | 75.80% (379/500) | -10.20 pp |
| Single-session user | 97.14% | 88.57% | -8.57 pp |
| Knowledge update | 91.03% | 89.74% | -1.29 pp |
| Temporal reasoning | 88.72% | 80.45% | -8.27 pp |
| Single-session preference | 80.00% | 53.33% | -26.67 pp |
| Multi-session | 79.70% | 63.91% | -15.79 pp |
| Single-session assistant | 76.79% | 69.64% | -7.15 pp |

The retrieval comparison explains most of the answer-accuracy gap:

| Retrieval metric | Oracle | LongMemEval-S |
|---|---:|---:|
| Evidence hit rate | 99.79% | 91.44% |
| Gold message recall | 98.05% | 84.95% |
| Injected chunk recall | 95.75% | 80.75% |
| Injected text coverage | 88.13% | 71.20% |
| Source Session recall | 100.00% | 98.44% |
| Evidence MRR | 0.701 | 0.504 |
| Average retrieved evidence | 27.95 | 36.86 |
| Average memory context | 1,717 tokens | 2,344 tokens |
| Average retrieval duration | 8.34 seconds | 9.48 seconds |

Of the 479 questions with labeled evidence, complete gold-message recall fell
from 456 oracle questions to 373 LongMemEval-S questions. When all gold messages
were present, answer accuracy was nearly unchanged: 88.16% on oracle and 87.67%
on LongMemEval-S. The 10.2-point overall difference therefore comes primarily
from exact evidence being missed, only partially recalled, or ranked too low in
the much larger haystack, rather than from a broad regression in answer-model
quality. The LongMemEval-S run completed 500 unique predictions with no runtime
or retrieval-channel errors; its 75.8% result is the current full-haystack
baseline and should not be compared directly with oracle-only scores.

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
| **LuminaCode (LongMemEval-S)** | **75.8%** | Full haystack; DeepSeek Judge; official prompt reused; 500 questions |
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
Exabase and Honcho
scores are included as publicly reported results with less complete reproduction
material. Reader, retrieval depth, context budget, and judge differ across
reports, so this table is not a strict apples-to-apples leaderboard.

Published LoCoMo LLM-Judge results:

| System | Overall | Multi-Hop | Temporal | Open Domain | Single-Hop |
|---|---:|---:|---:|---:|---:|
| [Attemory](https://github.com/AttemorySystem/attemory/blob/main/benchmarks/results/LoCoMo/report.txt) | 94.52% | 81.25% | 92.52% | 96.91% | 93.97% |
| [MemoryLake](https://github.com/memorylake-ai/memorylake-locomo-benchmark) | 94.03% | 91.84% | 91.28% | 85.42% | 96.79% |
| [EverMemOS](https://arxiv.org/abs/2601.02163) | 93.05% | 91.84% | 89.72% | 76.04% | 96.67% |
| [MemCog](https://arxiv.org/abs/2605.28046) | 92.98% | 80.21% | 92.81% | 94.89% | 91.84% |
| [Backboard](https://github.com/Backboard-io/Backboard-Locomo-Benchmark) | 90.00% | 75.00% | 91.90% | 91.20% | 89.36% |
| [Hindsight](https://github.com/vectorize-io/hindsight-benchmarks) | 89.61% | 70.83% | 83.80% | 95.12% | 86.17% |
| **LuminaCode** | **77.40%** | **55.21%** | **76.32%** | **82.05%** | **72.34%** |
| [Memobase v0.0.37](https://github.com/memodb-io/memobase/blob/main/docs/experiments/locomo-benchmark/README.md) | 75.78% | 46.88% | 85.05% | 77.17% | 70.92% |
| Zep | 75.14% | 66.04% | 79.79% | 67.71% | 74.11% |
| Mem0-Graph | 68.44% | 47.19% | 58.13% | 75.71% | 65.71% |
| Mem0 | 66.88% | 51.15% | 55.51% | 72.93% | 67.13% |

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

AppRoot v2 uses five ownership layers:

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
[AppRoot v2](docs/app-root-v2.md) for the complete storage and migration
contract.

macOS/Linux:

```sh
make install
```

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
