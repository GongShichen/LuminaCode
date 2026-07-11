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

### Storage And Ingestion

- A persistent message cursor commits visible messages before semantic
  enrichment. Each message is stored as an immutable evidence span, overlapping
  chunk, and sentence/list/code-aware evidence atom.
- Evidence atoms target 96 tokens and retain exact message IDs, Session IDs,
  roles, rune offsets, timestamps, content hashes, and scope metadata. Original
  messages and chunks remain available for reconstruction and re-indexing.
- Semantic enrichment produces memories, facts, entities, relations, canonical
  events, and core blocks. It references source message IDs instead of copying
  evidence text and runs through recoverable background jobs.
- SQLite FTS5 indexes memories, Sessions, chunks, and atoms. Local normalized
  embeddings are stored beside the source records, with content hashes making
  indexing and backfill idempotent.

### Retrieval And Evidence Assembly

- Every query runs six channels: BM25, local vector, entity, temporal, Session,
  and graph retrieval. Query expansion can add synonyms, entities, temporal
  hints, relation terms, and provenance hints, but it cannot select channels,
  scopes, weights, or memory types.
- BM25, vector, entity, temporal, and Session retrieval run concurrently. The
  graph channel expands their fused candidates, while selected Sessions receive
  an additional SQL-constrained atom search.
- Channel results are grouped into independent lexical, semantic, entity, time,
  Session, and relation signal families. Reciprocal-rank fusion counts the best
  contribution from each family once.
- The Evidence Ledger builds generic query facets and selects atoms by fused
  relevance, uncovered-facet gain, epistemic provenance, source contribution,
  entity/time coherence, and token cost. One residual all-channel sweep can add
  evidence for uncovered facets.
- The evidence packet places selected atoms first, merges atoms from the same
  message in source order, then adds bounded neighboring context. Core blocks
  use a separate budget. The packet retains source IDs, roles, timestamps,
  validity intervals, provenance, and timeline entries.
- Local ONNX inference uses one backend-wide scheduler with separate query and
  document queues, micro-batching, content/query caches, and execution-time
  accounting that starts after a batch receives an execution slot.

### Time, Provenance, And Isolation

- Each user turn has one reference timestamp shared by query expansion,
  temporal indexes, timeline assembly, and the hidden answering context.
- Facts use valid time (`valid_from` / `valid_until`) and observed time
  (`observed_at` / `invalidated_at`). Updates retain prior versions and connect
  them with `supersedes` and `contradicts` relations.
- Evidence records epistemic status as `reported`, `observed`, `derived`,
  `suggested`, `hypothetical`, or `questioned`. Canonical entities and events
  retain every source atom and its scope.
- User, project, Team, agent-type, and Team-agent scopes are checked by every
  retrieval channel and graph traversal. Retrieved evidence is transient hidden
  context and does not enter the visible transcript.

### Lifecycle And Governance

- Fact validity and storage retention are independent. Active memories move
  between `hot`, `warm`, and `cold` using access and reinforcement signals.
- Lifecycle value combines importance, confidence, access recency/frequency,
  reinforcement, provenance strength, and dependency strength. Expired,
  unpinned, low-value records can be archived after a grace period.
- Background maintenance never performs physical deletion. Pins, active
  dependencies, core blocks, and unresolved conflict chains are protected;
  archived records can be restored and re-indexed.

`make install` downloads `multilingual-e5-small` from ModelScope into
`~/.lumina/models/memory/`; `make uninstall` removes it. Use `/Memory`,
`/MemorySearch`, `/MemoryForget`, `/MemoryExport`, and `/MemoryImport` for local
governance.

Common tuning fields in `~/.lumina/CONFIG/defaults.json`:

| Setting | Default | Purpose |
|---|---:|---|
| `memory_session_candidates` | 12 | Relevant Sessions searched in depth |
| `memory_chunks_per_session` | 6 | Maximum fused chunks retained per Session |
| `memory_session_chunk_candidates` | 64 | Per-channel candidates inside a Session |
| `memory_atom_target_tokens` / `memory_atom_max_tokens` | 96 / 160 | Evidence atom size |
| `memory_atom_max_selected` | 32 | Safety cap; the token budget determines normal selection |
| `memory_coverage_max_facets` | 8 | Generic query facets tracked by the Evidence Ledger |
| `memory_coverage_completion_rounds` | 1 | Residual all-channel coverage sweep count |
| `memory_adjacent_chunk_window` | 1 | Neighbor atoms considered after direct evidence |
| `memory_embedding_batch_size` / `memory_embedding_batch_wait_ms` | 32 / 20 | Shared local embedding micro-batch |
| `memory_retrieval_cache_ttl_seconds` | 300 | Scope-safe retrieval cache lifetime |
| `memory_query_expansion_timeout_seconds` | 4 | Maximum wait for generic query expansion |
| `memory_lifecycle_enabled` | true | Enable temperature, scoring, and automatic archival |
| `memory_maintenance_interval_seconds` | 300 | Lifecycle maintenance interval |
| `memory_hot_access_days` / `memory_warm_access_days` | 30 / 90 | Hot, warm, and cold access windows |
| `memory_access_recency_half_life_days` | 30 | Half-life for access-recency decay |
| `memory_archive_grace_days` | 30 | Grace period after retention expires |
| `memory_archive_value_threshold` | 0.45 | Maximum value score eligible for archival |
| `memory_value_weights` | See example config | Seven lifecycle value weights |
| `memory_auto_hard_delete_enabled` | false | Must remain disabled; background hard delete is forbidden |

The five `memory_coverage_*_weight` values and three
`memory_evidence_*_budget_ratio` values must each add up to `1`. Deprecated
`memory_mmr_*` and `memory_recall_max_items` fields are accepted for compatibility
but no longer control evidence selection. Configuration is hot-reloaded for the
next turn; `make install` adds new defaults without overwriting existing values.

Lifecycle value combines importance, confidence, access recency and frequency,
reinforcement, provenance, and dependency strength. `memory_value_weights` must
be complete, non-negative, and sum to `1`. `memory_auto_hard_delete_enabled`
must remain `false`; physical deletion always requires an explicit user action.
The `/Memory` view exposes temperature, value, retention, archive reasons, and
lifecycle events, with pin, unpin, archive, and restore controls.

### LongMemEval

Lumina scored **83.2% (416/500)** on the 500-question oracle set. Answers were
evaluated with the official LongMemEval judge prompt and `deepseek-v4-pro`
through `https://api.deepseek.com`. This is a complete black-box run of the
production ingestion and retrieval path; it is not an official GPT-4o
leaderboard score.

| Question type | Accuracy |
|---|---:|
| Single-session user | 97.14% |
| Knowledge update | 92.31% |
| Single-session assistant | 87.50% |
| Temporal reasoning | 85.71% |
| Single-session preference | 73.33% |
| Multi-session | 68.42% |

The run also records retrieval quality separately from answer accuracy:

| Retrieval metric | Result |
|---|---:|
| Evidence hit rate | 98.54% |
| Evidence Recall@K | 95.26% |
| Evidence MRR | 0.374 |
| Source Session recall | 100.00% |
| Gold message recall | 95.99% |
| Injected chunk recall | 95.26% |
| Injected text coverage | 83.40% |
| Average memory context | 1,277 tokens (21.10% memory token ratio) |
| Average retrieval duration | 3.52 seconds |

Retrieval metrics are computed from the evidence atom IDs and source spans
actually injected into the answering model.

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
| **LuminaCode** | **83.2%** | DeepSeek Judge, official prompt reused; 500 questions |
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
