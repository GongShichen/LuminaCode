# LuminaCode

[简体中文](README.zh-CN.md)

LuminaCode is a local general-purpose Agent. It uses a Go backend and a
TypeScript TUI, with project context, tools, skills, MCP, memory, sessions, and
Agent Teams.

## Agent Capabilities

- Uses the launch directory as the default project root.
- Reads `LUMINA.md` / `AGENTS.md`, with user-level fallback under `~/.lumina`.
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
~/.lumina/run/backend.json
```

The backend listens on `127.0.0.1`, requires an auth token, supports multiple
sessions, and serializes submits within each session. Headless paths are handled
by `lumina-backend`.

## Long-Term Memory

Cross-session memory lives in one local SQLite store:

```text
~/.lumina/memory/lumina-memory.sqlite
```

- Ingestion first commits original messages and overlapping evidence chunks with
  a persistent cursor. Semantic facts, entities, temporal versions, and
  relations are enriched independently and can resume after a restart.
- Every query runs BM25, local vector, entity, temporal, Session, and graph
  retrieval. Results are fused with RRF, deduplicated with MMR, and packed from
  the original chunks into a small sourced evidence packet.
- User, project, Team, agent-type, and Team-agent scopes are isolated. Retrieved
  evidence is transient context and never enters the visible transcript.
- Facts retain valid-time and observed-time history, so updates supersede old
  values without deleting their sources.

`make install` downloads `multilingual-e5-small` from ModelScope into
`~/.lumina/models/memory/`; `make uninstall` removes it. Use `/Memory`,
`/MemorySearch`, `/MemoryForget`, `/MemoryExport`, and `/MemoryImport` for local
governance.

### LongMemEval

Lumina scored **68.4% (342/500)** on the 500-question oracle set. The saved
answers were evaluated with the official LongMemEval judge prompt and
`deepseek-v4-pro`. This is not an official GPT-4o leaderboard score.

The run also records retrieval quality separately from answer accuracy:

| Retrieval metric | Result |
|---|---:|
| Evidence hit rate | 82.7% |
| Evidence Recall@K | 68.6% |
| Evidence MRR | 0.438 |
| Source Session recall | 97.2% |
| Gold message recall | 69.0% |
| Injected chunk recall | 68.6% |
| Injected text coverage | 69.1% |
| Average memory context | 1,819 tokens (23.9% of input) |

These metrics are measured from the evidence chunks actually injected into the
answering model. They show the remaining gap clearly: the correct Session is
usually found, but selecting the exact supporting messages across Sessions is
still the main retrieval bottleneck.

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
| LiCoMemory | 73.8% | GPT-4o-mini, five-run mean |
| **LuminaCode** | **68.4%** | DeepSeek Judge, official prompt reused |
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
{session_dir}/{parent_session_id}/teams/{team_session_id}/
{session_dir}/{parent_session_id}/teams/{team_session_id}/agents/{agent_id}/
~/.lumina/project/{project_root_name}/teams/{team_name}/{team_session_id}/
```

### Built-in Teams

Installed under `~/.lumina/TEAM/`:

- `product-development`: full-stack delivery with `team-leader`, `research`,
  `frontend`, `backend`, `qa`, `reviewer`, `devops`, and `ux-design`. Uses
  contract, QA, reviewer, task-policy, and follow-up/deferral gates.
- `deep-research`: research team with `team-leader`, `scope-planner`,
  `search-strategist`, `source-reader`, `evidence-analyst`, `report-writer`,
  `qa`, and `reviewer`. Uses SearxNG `WebSearch` / `WebFetch` and arXiv MCP;
  can export report and evidence files.

### Creating a Team

`/NewTeam` asks for a display name and creates:

```text
~/.lumina/TEAM/{team_name}/
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
`~/.lumina/CONFIG/defaults.json`:

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
3. `~/.lumina/LUMINA.md`
4. `~/.lumina/AGENTS.md`

All files are optional.

## Skills

Skills are `SKILL.md` instruction packages loaded from:

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`
- `~/.lumina/skills/`
- `~/.lumina/SKILLS/`

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
~/.lumina/project/{project_root_name}/CONFIG/trusted_mcp.json
```

Use `/mcp` to inspect registered MCP tools.

## Sessions and Runtime Data

Installed resources:

```text
~/.lumina/
```

- `CONFIG/defaults.json`
- `SYSTEM/system-prompt.md`
- `SYSTEM/extraction_system.md`
- `SKILLS/`
- `TEAM/`
- `frontend/`

Project runtime data:

```text
~/.lumina/project/{project_root_name}/
```

- `CONFIG/trusted_mcp.json`
- `background/tool-results/`

Project-authored resources:

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`

Session history uses `session_dir` (`~/.lumina/sessions` by default):

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
~/.lumina/project/{project_root_name}/background/tool-results/
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

macOS/Linux:

```sh
make install
```

Windows:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\install-windows.ps1
```

Doctor:

```sh
make doctor
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
`~/.lumina`, and leaves shell rc files plus project-local `.Lumina` untouched.

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
